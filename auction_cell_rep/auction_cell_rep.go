package auction_cell_rep

import (
	"errors"
	"net/url"
	"strconv"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/executor"
	"github.com/cloudfoundry-incubator/rep"
	"github.com/cloudfoundry-incubator/rep/evacuation/evacuation_context"
	"github.com/pivotal-golang/lager"
)

var ErrPreloadedRootFSNotFound = errors.New("preloaded rootfs path not found")

type AuctionCellRep struct {
	cellID               string
	stackPathMap         rep.StackPathMap
	rootFSProviders      rep.RootFSProviders
	stack                string
	zone                 string
	generateInstanceGuid func() (string, error)
	client               executor.Client
	evacuationReporter   evacuation_context.EvacuationReporter
	logger               lager.Logger
}

func New(
	cellID string,
	preloadedStackPathMap rep.StackPathMap,
	arbitraryRootFSes []string,
	zone string,
	generateInstanceGuid func() (string, error),
	client executor.Client,
	evacuationReporter evacuation_context.EvacuationReporter,
	logger lager.Logger,
) *AuctionCellRep {
	return &AuctionCellRep{
		cellID:               cellID,
		stackPathMap:         preloadedStackPathMap,
		rootFSProviders:      rootFSProviders(preloadedStackPathMap, arbitraryRootFSes),
		zone:                 zone,
		generateInstanceGuid: generateInstanceGuid,
		client:               client,
		evacuationReporter:   evacuationReporter,
		logger:               logger.Session("auction-delegate"),
	}
}

func rootFSProviders(preloaded rep.StackPathMap, arbitrary []string) rep.RootFSProviders {
	rootFSProviders := rep.RootFSProviders{}
	for _, scheme := range arbitrary {
		rootFSProviders[scheme] = rep.ArbitraryRootFSProvider{}
	}

	stacks := make([]string, 0, len(preloaded))
	for stack, _ := range preloaded {
		stacks = append(stacks, stack)
	}
	rootFSProviders["preloaded"] = rep.NewFixedSetRootFSProvider(stacks...)

	return rootFSProviders
}

func PathForRootFS(rootFS string, stackPathMap rep.StackPathMap) (string, error) {
	if rootFS == "" {
		return rootFS, nil
	}

	url, err := url.Parse(rootFS)
	if err != nil {
		return "", err
	}

	if url.Scheme == models.PreloadedRootFSScheme {
		path, ok := stackPathMap[url.Opaque]
		if !ok {
			return "", ErrPreloadedRootFSNotFound
		}
		return path, nil
	}

	return rootFS, nil
}

func taskKeyFromTags(tags executor.Tags) (guid string, domain string, err error) {
	if tags == nil {
		return "", "", rep.ErrContainerMissingTags
	}

	guid = tags[rep.ProcessGuidTag]
	domain = tags[rep.DomainTag]

	if guid == "" || domain == "" {
		return "", "", rep.ErrContainerMissingTags
	}

	return
}
// State currently does not return tasks or lrp rootfs, because the
// auctioneer currently does not need them.
func (a *AuctionCellRep) State() (rep.CellState, error) {
	logger := a.logger.Session("auction-state")
	logger.Info("providing")

	totalResources, err := a.fetchResourcesVia(a.client.TotalResources)
	if err != nil {
		logger.Error("failed-to-get-total-resources", err)
		return rep.CellState{}, err
	}

	availableResources, err := a.fetchResourcesVia(a.client.RemainingResources)
	if err != nil {
		logger.Error("failed-to-get-remaining-resource", err)
		return rep.CellState{}, err
	}

	containers, err := a.client.ListContainers(nil)

	if err != nil {
		logger.Error("failed-to-fetch-containers", err)
		return rep.CellState{}, err
	}

	var taskGuid, taskDomain string
	var key *models.ActualLRPKey
	var keyErr error
	tasks := []rep.Task{}
	lrps := []rep.LRP{}
	for i := range containers {
		container := &containers[i]
		resource := rep.Resource{MemoryMB: int32(container.MemoryMB), DiskMB: int32(container.DiskMB)}
		if container.Tags[rep.LifecycleTag] == rep.TaskLifecycle {
			taskGuid, taskDomain, keyErr = taskKeyFromTags(container.Tags)
			if keyErr != nil {
				logger.Error("failed-to-extract-task key", keyErr)
				continue
			}
			tasks = append(tasks, rep.NewTask(taskGuid, taskDomain, resource))
		} else {
			key, keyErr = rep.ActualLRPKeyFromTags(container.Tags)
			if keyErr != nil {
				logger.Error("failed-to-extract-key", keyErr)
				continue
			}
			lrps = append(lrps, rep.NewLRP(*key, resource))
		}
	}

	state := rep.NewCellState(a.rootFSProviders, availableResources, totalResources, lrps, tasks, a.zone, a.evacuationReporter.Evacuating())

	a.logger.Info("provided", lager.Data{
		"available-resources": state.AvailableResources,
		"total-resources":     state.TotalResources,
		"num-lrps":            len(state.LRPs),
		"zone":                state.Zone,
		"evacuating":          state.Evacuating,
	})

	return state, nil
}

func (a *AuctionCellRep) Perform(work rep.Work) (rep.Work, error) {
	var failedWork = rep.Work{}

	logger := a.logger.Session("auction-work", lager.Data{
		"lrp-starts": len(work.LRPs),
		"tasks":      len(work.Tasks),
	})

	if a.evacuationReporter.Evacuating() {
		return work, nil
	}

	if len(work.LRPs) > 0 {
		lrpLogger := logger.Session("lrp-allocate-instances")

		requests, lrpMap, untranslatedLRPs := a.lrpsToAllocationRequest(work.LRPs)
		if len(untranslatedLRPs) > 0 {
			lrpLogger.Info("failed-to-translate-lrps-to-containers", lager.Data{"num-failed-to-translate": len(untranslatedLRPs)})
			failedWork.LRPs = untranslatedLRPs
		}

		lrpLogger.Info("requesting-container-allocation", lager.Data{"num-requesting-allocation": len(requests)})
		failures, err := a.client.AllocateContainers(requests)
		if err != nil {
			lrpLogger.Error("failed-requesting-container-allocation", err)
			failedWork.LRPs = work.LRPs
		} else {
			lrpLogger.Info("succeeded-requesting-container-allocation", lager.Data{"num-failed-to-allocate": len(failures)})
			for i := range failures {
				failure := &failures[i]
				lrpLogger.Error("container-allocation-failure", failure, lager.Data{"failed-request": &failure.AllocationRequest})
				if lrp, found := lrpMap[failure.Guid]; found {
					failedWork.LRPs = append(failedWork.LRPs, *lrp)
				}
			}
		}
	}

	if len(work.Tasks) > 0 {
		taskLogger := logger.Session("task-allocate-instances")

		requests, taskMap, failedTasks := a.tasksToAllocationRequests(work.Tasks)
		if len(failedTasks) > 0 {
			taskLogger.Info("failed-to-translate-tasks-to-containers", lager.Data{"num-failed-to-translate": len(failedTasks)})
			failedWork.Tasks = failedTasks
		}

		taskLogger.Info("requesting-container-allocation", lager.Data{"num-requesting-allocation": len(requests)})
		failures, err := a.client.AllocateContainers(requests)
		if err != nil {
			taskLogger.Error("failed-requesting-container-allocation", err)
			failedWork.Tasks = work.Tasks
		} else {
			taskLogger.Info("succeeded-requesting-container-allocation", lager.Data{"num-failed-to-allocate": len(failures)})
			for i := range failures {
				failure := &failures[i]
				taskLogger.Error("container-allocation-failure", failure, lager.Data{"failed-request": &failure.AllocationRequest})
				if task, found := taskMap[failure.Guid]; found {
					failedWork.Tasks = append(failedWork.Tasks, *task)
				}
			}
		}
	}

	return failedWork, nil
}

func (a *AuctionCellRep) lrpsToAllocationRequest(lrps []rep.LRP) ([]executor.AllocationRequest, map[string]*rep.LRP, []rep.LRP) {
	requests := make([]executor.AllocationRequest, 0, len(lrps))
	untranslatedLRPs := make([]rep.LRP, 0)
	lrpMap := make(map[string]*rep.LRP, len(lrps))
	for i := range lrps {
		lrp := &lrps[i]
		tags := executor.Tags{}

		instanceGuid, err := a.generateInstanceGuid()
		if err != nil {
			untranslatedLRPs = append(untranslatedLRPs, *lrp)
			continue
		}

		tags[rep.DomainTag] = lrp.Domain
		tags[rep.ProcessGuidTag] = lrp.ProcessGuid
		tags[rep.ProcessIndexTag] = strconv.Itoa(int(lrp.Index))
		tags[rep.LifecycleTag] = rep.LRPLifecycle
		tags[rep.InstanceGuidTag] = instanceGuid

		rootFSPath, err := PathForRootFS(lrp.RootFs, a.stackPathMap)
		if err != nil {
			untranslatedLRPs = append(untranslatedLRPs, *lrp)
			continue
		}

		containerGuid := rep.LRPContainerGuid(lrp.ProcessGuid, instanceGuid)
		lrpMap[containerGuid] = lrp

		resource := executor.NewResource(int(lrp.MemoryMB), int(lrp.DiskMB), rootFSPath)
		requests = append(requests, executor.NewAllocationRequest(containerGuid, &resource, tags))
	}

	return requests, lrpMap, untranslatedLRPs
}

func (a *AuctionCellRep) tasksToAllocationRequests(tasks []rep.Task) ([]executor.AllocationRequest, map[string]*rep.Task, []rep.Task) {
	failedTasks := make([]rep.Task, 0)
	taskMap := make(map[string]*rep.Task, len(tasks))
	requests := make([]executor.AllocationRequest, 0, len(tasks))

	for i := range tasks {
		task := &tasks[i]
		taskMap[task.TaskGuid] = task
		rootFSPath, err := PathForRootFS(task.RootFs, a.stackPathMap)
		if err != nil {
			failedTasks = append(failedTasks, *task)
			continue
		}
		tags := executor.Tags{}
		tags[rep.LifecycleTag] = rep.TaskLifecycle
		tags[rep.DomainTag] = task.Domain
		tags[rep.ProcessGuidTag] = task.TaskGuid

		resource := executor.NewResource(int(task.MemoryMB), int(task.DiskMB), rootFSPath)
		requests = append(requests, executor.NewAllocationRequest(task.TaskGuid, &resource, tags))
	}

	return requests, taskMap, failedTasks
}

func (a *AuctionCellRep) fetchResourcesVia(fetcher func() (executor.ExecutorResources, error)) (rep.Resources, error) {
	resources, err := fetcher()
	if err != nil {
		return rep.Resources{}, err
	}
	return rep.Resources{
		MemoryMB:   int32(resources.MemoryMB),
		DiskMB:     int32(resources.DiskMB),
		Containers: resources.Containers,
	}, nil
}
