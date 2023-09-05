package contract

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/interfaces"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/lib"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources/hashrate/allocator"
	"gitlab.com/TitanInd/proxy/proxy-router-v3/internal/resources/hashrate/hashrate"
	"golang.org/x/exp/slices"
)

var (
	ErrContractClosed = errors.New("contract closed")
)

type ContractWatcher struct {
	data *resources.ContractData

	state                 resources.ContractState
	fullMiners            []string
	actualHRGHS           *hashrate.Hashrate
	fulfillmentStartedAt  *time.Time
	contractCycleDuration time.Duration

	tsk *lib.Task

	//deps
	allocator *allocator.Allocator
	log       interfaces.ILogger
}

const (
	ResourceTypeHashrate        = "hashrate"
	ResourceEstimateHashrateGHS = "hashrate_ghs"
)

func NewContractWatcherSeller(data *resources.ContractData, cycleDuration time.Duration, hashrateFactory func() *hashrate.Hashrate, allocator *allocator.Allocator, log interfaces.ILogger) *ContractWatcher {
	return &ContractWatcher{
		data:                  data,
		state:                 resources.ContractStatePending,
		allocator:             allocator,
		fullMiners:            []string{},
		contractCycleDuration: cycleDuration,
		actualHRGHS:           hashrateFactory(),
		log:                   log,
	}
}

func (p *ContractWatcher) StartFulfilling(ctx context.Context) {
	p.log.Infof("contract started fulfilling")
	startedAt := time.Now()
	p.fulfillmentStartedAt = &startedAt
	p.state = resources.ContractStateRunning
	p.tsk = lib.NewTaskFunc(p.Run)
	p.tsk.Start(ctx)
}

func (p *ContractWatcher) StopFulfilling() {
	<-p.tsk.Stop()
	p.log.Infof("contract stopped fulfilling")
}

func (p *ContractWatcher) Done() <-chan struct{} {
	return p.tsk.Done()
}

func (p *ContractWatcher) Err() error {
	if errors.Is(p.tsk.Err(), context.Canceled) {
		return ErrContractClosed
	}
	return p.tsk.Err()
}

func (p *ContractWatcher) SetData(data *resources.ContractData) {
	p.data = data
}

// Run is the main loop of the contract. It is responsible for allocating miners for the contract.
// Returns nil if the contract ended successfully, ErrClosed if the contract was closed before it ended.
func (p *ContractWatcher) Run(ctx context.Context) error {
	partialDeliveryTargetGHS := p.GetHashrateGHS()
	thisCycleJobSubmitted := atomic.Uint64{}
	thisCyclePartialAllocation := 0.0

	onSubmit := func(diff float64, minerID string) {
		p.log.Infof("contract submit %s, %.0f, total work %d", minerID, diff, thisCycleJobSubmitted.Load())
		p.actualHRGHS.OnSubmit(diff)
		thisCycleJobSubmitted.Add(uint64(diff))
		// TODO: catch overdelivery here and cancel tasks
	}

	for {
		p.log.Debugf("new contract cycle:  partialDeliveryTargetGHS=%.1f, thisCyclePartialAllocation=%.0f",
			partialDeliveryTargetGHS, thisCyclePartialAllocation,
		)
		if partialDeliveryTargetGHS > 0 {
			fullMiners, newRemainderGHS := p.allocator.AllocateFullMinersForHR(partialDeliveryTargetGHS, p.data.Dest, p.GetDuration(), onSubmit)
			if len(fullMiners) > 0 {
				partialDeliveryTargetGHS = newRemainderGHS
				p.log.Infof("fully allocated %d miners, new partialDeliveryTargetGHS = %.1f", len(fullMiners), partialDeliveryTargetGHS)
				p.fullMiners = append(p.fullMiners, fullMiners...)
			} else {
				p.log.Debugf("no full miners were allocated for this contract")
			}

			thisCyclePartialAllocation = partialDeliveryTargetGHS
			minerID, ok := p.allocator.AllocatePartialForHR(partialDeliveryTargetGHS, p.data.Dest, p.contractCycleDuration, onSubmit)
			if ok {
				p.log.Debugf("remainderGHS: %.1f, was allocated by partial miners %v", partialDeliveryTargetGHS, minerID)
			} else {
				p.log.Warnf("remainderGHS: %.1f, was not allocated by partial miners", partialDeliveryTargetGHS)
			}
		}

		// in case of too much hashrate
		if partialDeliveryTargetGHS < 0 {
			p.log.Info("removing least powerful miner from contract")
			var items []*allocator.MinerItem
			for _, minerID := range p.fullMiners {
				miner, ok := p.allocator.GetMiners().Load(minerID)
				if !ok {
					continue
				}
				items = append(items, &allocator.MinerItem{
					ID:    miner.GetID(),
					HrGHS: miner.HashrateGHS(),
				})
			}

			if len(items) > 0 {
				slices.SortStableFunc(items, func(a, b *allocator.MinerItem) bool {
					return b.HrGHS > a.HrGHS
				})
				minerToRemove := items[0].ID
				miner, ok := p.allocator.GetMiners().Load(minerToRemove)
				if ok {
					// replace with more specific remove (by tag which could be a contractID)
					miner.ResetTasks()
					p.log.Debugf("miner %s tasks removed", miner.GetID())
					// TODO: remove from full miners
					newFullMiners := make([]string, len(p.fullMiners)-1)
					i := 0
					for _, minerID := range p.fullMiners {
						if minerID == minerToRemove {
							continue
						}
						newFullMiners[i] = minerID
						i++
					}
					p.fullMiners = newFullMiners

					// sets new target and restarts the cycle
					partialDeliveryTargetGHS = miner.HashrateGHS() + partialDeliveryTargetGHS
					continue
				}
			} else {
				p.log.Warnf("no miners found to be removed")
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(*p.GetEndTime())):
			expectedJob := hashrate.GHSToJobSubmitted(p.GetHashrateGHS()) * p.GetDuration().Seconds()
			actualJob := p.actualHRGHS.GetTotalWork()
			undeliveredJob := expectedJob - float64(actualJob)
			undeliveredFraction := undeliveredJob / expectedJob

			for _, minerID := range p.fullMiners {
				miner, ok := p.allocator.GetMiners().Load(minerID)
				if !ok {
					continue
				}
				miner.ResetTasks()
				p.log.Debugf("miner %s tasks removed", miner.GetID())
			}
			p.fullMiners = p.fullMiners[:0]

			// partial miners tasks are not reset because they are not allocated
			// for the full duration of the contract

			p.log.Infof("contract ended, undelivered work %d, undelivered fraction %.2f",
				int(undeliveredJob), undeliveredFraction)
			return nil
		case <-time.After(p.contractCycleDuration):
		}

		thisCycleActualGHS := hashrate.JobSubmittedToGHS(float64(thisCycleJobSubmitted.Load()) / p.contractCycleDuration.Seconds())
		thisCycleUnderDeliveryGHS := p.GetHashrateGHS() - thisCycleActualGHS

		// plan for the next cycle is to compensate for the under delivery of this cycle
		partialDeliveryTargetGHS = partialDeliveryTargetGHS + thisCycleUnderDeliveryGHS

		thisCycleJobSubmitted.Store(0)

		p.log.Infof(
			"contract cycle ended, thisCycleActualGHS = %.1f, thisCycleUnderDeliveryGHS=%.1f, partialDeliveryTargetGHS=%.1f",
			thisCycleActualGHS, thisCycleUnderDeliveryGHS, partialDeliveryTargetGHS,
		)
	}
}

func (p *ContractWatcher) GetRole() resources.ContractRole {
	return resources.ContractRoleSeller
}

func (p *ContractWatcher) GetDest() string {
	return p.data.Dest.String()
}

func (p *ContractWatcher) GetDuration() time.Duration {
	return p.data.Duration
}

func (p *ContractWatcher) GetEndTime() *time.Time {
	if p.data.StartedAt == nil {
		return nil
	}
	endTime := p.data.StartedAt.Add(p.data.Duration)
	return &endTime
}

func (p *ContractWatcher) GetFulfillmentStartedAt() *time.Time {
	return p.fulfillmentStartedAt
}

func (p *ContractWatcher) GetID() string {
	return p.data.ContractID
}

func (p *ContractWatcher) GetHashrateGHS() float64 {
	return p.data.ResourceEstimates[ResourceEstimateHashrateGHS]
}

func (p *ContractWatcher) GetResourceEstimates() map[string]float64 {
	return p.data.ResourceEstimates
}

func (p *ContractWatcher) GetResourceEstimatesActual() map[string]float64 {
	return p.actualHRGHS.GetHashrateAvgGHSAll()
}

func (p *ContractWatcher) GetResourceType() string {
	return ResourceTypeHashrate
}

func (p *ContractWatcher) GetSeller() string {
	return p.data.Seller
}

func (p *ContractWatcher) GetBuyer() string {
	return p.data.Buyer
}

func (p *ContractWatcher) GetStartedAt() *time.Time {
	return p.data.StartedAt
}

func (p *ContractWatcher) GetState() resources.ContractState {
	return p.state
}
