/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2019 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package executor

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	null "gopkg.in/guregu/null.v3"

	"github.com/loadimpact/k6/lib"
	"github.com/loadimpact/k6/lib/types"
	"github.com/loadimpact/k6/stats"
	"github.com/loadimpact/k6/ui/pb"
)

const variableArrivalRateType = "variable-arrival-rate"

func init() {
	lib.RegisterExecutorConfigType(
		variableArrivalRateType,
		func(name string, rawJSON []byte) (lib.ExecutorConfig, error) {
			config := NewVariableArrivalRateConfig(name)
			err := lib.StrictJSONUnmarshal(rawJSON, &config)
			return config, err
		},
	)
}

// VariableArrivalRateConfig stores config for the variable arrival-rate executor
type VariableArrivalRateConfig struct {
	BaseConfig
	StartRate null.Int           `json:"startRate"`
	TimeUnit  types.NullDuration `json:"timeUnit"`
	Stages    []Stage            `json:"stages"`

	// Initialize `PreAllocatedVUs` number of VUs, and if more than that are needed,
	// they will be dynamically allocated, until `MaxVUs` is reached, which is an
	// absolutely hard limit on the number of VUs the executor will use
	PreAllocatedVUs null.Int `json:"preAllocatedVUs"`
	MaxVUs          null.Int `json:"maxVUs"`
}

// NewVariableArrivalRateConfig returns a VariableArrivalRateConfig with default values
func NewVariableArrivalRateConfig(name string) VariableArrivalRateConfig {
	return VariableArrivalRateConfig{
		BaseConfig: NewBaseConfig(name, variableArrivalRateType),
		TimeUnit:   types.NewNullDuration(1*time.Second, false),
	}
}

// Make sure we implement the lib.ExecutorConfig interface
var _ lib.ExecutorConfig = &VariableArrivalRateConfig{}

// GetPreAllocatedVUs is just a helper method that returns the scaled pre-allocated VUs.
func (varc VariableArrivalRateConfig) GetPreAllocatedVUs(et *lib.ExecutionTuple) int64 {
	return et.ES.Scale(varc.PreAllocatedVUs.Int64)
}

// GetMaxVUs is just a helper method that returns the scaled max VUs.
func (varc VariableArrivalRateConfig) GetMaxVUs(et *lib.ExecutionTuple) int64 {
	return et.ES.Scale(varc.MaxVUs.Int64)
}

// GetDescription returns a human-readable description of the executor options
func (varc VariableArrivalRateConfig) GetDescription(et *lib.ExecutionTuple) string {
	//TODO: something better? always show iterations per second?
	maxVUsRange := fmt.Sprintf("maxVUs: %d", et.ES.Scale(varc.PreAllocatedVUs.Int64))
	if varc.MaxVUs.Int64 > varc.PreAllocatedVUs.Int64 {
		maxVUsRange += fmt.Sprintf("-%d", et.ES.Scale(varc.MaxVUs.Int64))
	}
	maxUnscaledRate := getStagesUnscaledMaxTarget(varc.StartRate.Int64, varc.Stages)
	maxArrRatePerSec, _ := getArrivalRatePerSec(
		getScaledArrivalRate(et.ES, maxUnscaledRate, time.Duration(varc.TimeUnit.Duration)),
	).Float64()

	return fmt.Sprintf("Up to %.2f iterations/s for %s over %d stages%s",
		maxArrRatePerSec, sumStagesDuration(varc.Stages),
		len(varc.Stages), varc.getBaseInfo(maxVUsRange))
}

// Validate makes sure all options are configured and valid
func (varc VariableArrivalRateConfig) Validate() []error {
	errors := varc.BaseConfig.Validate()

	if varc.StartRate.Int64 < 0 {
		errors = append(errors, fmt.Errorf("the startRate value shouldn't be negative"))
	}

	if time.Duration(varc.TimeUnit.Duration) < 0 {
		errors = append(errors, fmt.Errorf("the timeUnit should be more than 0"))
	}

	errors = append(errors, validateStages(varc.Stages)...)

	if !varc.PreAllocatedVUs.Valid {
		errors = append(errors, fmt.Errorf("the number of preAllocatedVUs isn't specified"))
	} else if varc.PreAllocatedVUs.Int64 < 0 {
		errors = append(errors, fmt.Errorf("the number of preAllocatedVUs shouldn't be negative"))
	}

	if !varc.MaxVUs.Valid {
		errors = append(errors, fmt.Errorf("the number of maxVUs isn't specified"))
	} else if varc.MaxVUs.Int64 < varc.PreAllocatedVUs.Int64 {
		errors = append(errors, fmt.Errorf("maxVUs shouldn't be less than preAllocatedVUs"))
	}

	return errors
}

// GetExecutionRequirements returns the number of required VUs to run the
// executor for its whole duration (disregarding any startTime), including the
// maximum waiting time for any iterations to gracefully stop. This is used by
// the execution scheduler in its VU reservation calculations, so it knows how
// many VUs to pre-initialize.
func (varc VariableArrivalRateConfig) GetExecutionRequirements(et *lib.ExecutionTuple) []lib.ExecutionStep {
	return []lib.ExecutionStep{
		{
			TimeOffset:      0,
			PlannedVUs:      uint64(et.ES.Scale(varc.PreAllocatedVUs.Int64)),
			MaxUnplannedVUs: uint64(et.ES.Scale(varc.MaxVUs.Int64 - varc.PreAllocatedVUs.Int64)),
		},
		{
			TimeOffset:      sumStagesDuration(varc.Stages) + time.Duration(varc.GracefulStop.Duration),
			PlannedVUs:      0,
			MaxUnplannedVUs: 0,
		},
	}
}

// NewExecutor creates a new VariableArrivalRate executor
func (varc VariableArrivalRateConfig) NewExecutor(
	es *lib.ExecutionState, logger *logrus.Entry,
) (lib.Executor, error) {
	return VariableArrivalRate{
		BaseExecutor: NewBaseExecutor(varc, es, logger),
		config:       varc,
	}, nil
}

// HasWork reports whether there is any work to be done for the given execution segment.
func (varc VariableArrivalRateConfig) HasWork(et *lib.ExecutionTuple) bool {
	return varc.GetMaxVUs(et) > 0
}

// VariableArrivalRate tries to execute a specific number of iterations for a
// specific period.
//TODO: combine with the ConstantArrivalRate?
type VariableArrivalRate struct {
	*BaseExecutor
	config VariableArrivalRateConfig
}

// Make sure we implement the lib.Executor interface.
var _ lib.Executor = &VariableArrivalRate{}

func (varc VariableArrivalRateConfig) cal(et *lib.ExecutionTuple, ch chan<- time.Duration) {
	start, offsets, _ := et.GetStripedOffsets(et.ES)
	timeUnit := time.Duration(varc.TimeUnit.Duration).Nanoseconds()
	var state = newStageTransitionCalculator(start, offsets, varc.StartRate.ValueOrZero(), timeUnit, varc.Stages)

	go func() {
		defer close(ch) // TODO: maybe this is not a good design - closing a channel we receive
		state.loop(func(t time.Duration) { ch <- t })
	}()
}

// Run executes a variable number of iterations per second.
func (varr VariableArrivalRate) Run(ctx context.Context, out chan<- stats.SampleContainer) (err error) { //nolint:funlen
	segment := varr.executionState.ExecutionTuple.ES
	gracefulStop := varr.config.GetGracefulStop()
	duration := sumStagesDuration(varr.config.Stages)
	preAllocatedVUs := varr.config.GetPreAllocatedVUs(varr.executionState.ExecutionTuple)
	maxVUs := varr.config.GetMaxVUs(varr.executionState.ExecutionTuple)

	// TODO: refactor and simplify
	timeUnit := time.Duration(varr.config.TimeUnit.Duration)
	startArrivalRate := getScaledArrivalRate(segment, varr.config.StartRate.Int64, timeUnit)
	maxUnscaledRate := getStagesUnscaledMaxTarget(varr.config.StartRate.Int64, varr.config.Stages)
	maxArrivalRatePerSec, _ := getArrivalRatePerSec(getScaledArrivalRate(segment, maxUnscaledRate, timeUnit)).Float64()
	startTickerPeriod := getTickerPeriod(startArrivalRate)

	startTime, maxDurationCtx, regDurationCtx, cancel := getDurationContexts(ctx, duration, gracefulStop)
	defer cancel()

	// Make sure the log and the progress bar have accurate information
	varr.logger.WithFields(logrus.Fields{
		"maxVUs": maxVUs, "preAllocatedVUs": preAllocatedVUs, "duration": duration, "numStages": len(varr.config.Stages),
		"startTickerPeriod": startTickerPeriod.Duration, "type": varr.config.GetType(),
	}).Debug("Starting executor run...")

	// Pre-allocate the VUs local shared buffer
	vus := make(chan lib.VU, maxVUs)

	initialisedVUs := uint64(0)
	// Make sure we put back planned and unplanned VUs back in the global
	// buffer, and as an extra incentive, this replaces a waitgroup.
	defer func() {
		// no need for atomics, since initialisedVUs is mutated only in the select{}
		for i := uint64(0); i < initialisedVUs; i++ {
			varr.executionState.ReturnVU(<-vus, true)
		}
	}()

	// Get the pre-allocated VUs in the local buffer
	for i := int64(0); i < preAllocatedVUs; i++ {
		var vu lib.VU
		vu, err = varr.executionState.GetPlannedVU(varr.logger, true)
		if err != nil {
			return err
		}
		initialisedVUs++
		vus <- vu
	}

	tickerPeriod := int64(startTickerPeriod.Duration)

	vusFmt := pb.GetFixedLengthIntFormat(maxVUs)
	itersFmt := pb.GetFixedLengthFloatFormat(maxArrivalRatePerSec, 0) + " iters/s"

	progresFn := func() (float64, []string) {
		currentInitialisedVUs := atomic.LoadUint64(&initialisedVUs)
		currentTickerPeriod := atomic.LoadInt64(&tickerPeriod)
		vusInBuffer := uint64(len(vus))
		progVUs := fmt.Sprintf(vusFmt+"/"+vusFmt+" VUs",
			currentInitialisedVUs-vusInBuffer, currentInitialisedVUs)

		itersPerSec := 0.0
		if currentTickerPeriod > 0 {
			itersPerSec = float64(time.Second) / float64(currentTickerPeriod)
		}
		progIters := fmt.Sprintf(itersFmt, itersPerSec)

		right := []string{progVUs, duration.String(), progIters}

		spent := time.Since(startTime)
		if spent > duration {
			return 1, right
		}

		spentDuration := pb.GetFixedLengthDuration(spent, duration)
		progDur := fmt.Sprintf("%s/%s", spentDuration, duration)
		right[1] = progDur

		return math.Min(1, float64(spent)/float64(duration)), right
	}

	varr.progress.Modify(pb.WithProgress(progresFn))
	go trackProgress(ctx, maxDurationCtx, regDurationCtx, varr, progresFn)

	regDurationDone := regDurationCtx.Done()
	runIteration := getIterationRunner(varr.executionState, varr.logger, out)

	remainingUnplannedVUs := maxVUs - preAllocatedVUs

	var timer = time.NewTimer(time.Hour)
	var start = time.Now()
	var ch = make(chan time.Duration, 10) // buffer 10 iteration times ahead
	var prevTime time.Duration
	varr.config.cal(varr.executionState.ExecutionTuple, ch)
	for nextTime := range ch {
		select {
		case <-regDurationDone:
			return nil
		default:
		}
		atomic.StoreInt64(&tickerPeriod, int64(nextTime-prevTime))
		prevTime = nextTime
		b := time.Until(start.Add(nextTime))
		if b > 0 { // TODO: have a minimal ?
			timer.Reset(b)
			select {
			case <-timer.C:
			case <-regDurationDone:
				return nil
			}
		}

		var vu lib.VU
		select {
		case vu = <-vus:
			// ideally, we get the VU from the buffer without any issues
		default:
			if remainingUnplannedVUs == 0 {
				//TODO: emit an error metric?
				varr.logger.Warningf("Insufficient VUs, reached %d active VUs and cannot allocate more", maxVUs)
				continue
			}
			vu, err = varr.executionState.GetUnplannedVU(maxDurationCtx, varr.logger)
			if err != nil {
				return err
			}
			remainingUnplannedVUs--
			atomic.AddUint64(&initialisedVUs, 1)
		}
		go func(vu lib.VU) {
			runIteration(maxDurationCtx, vu)
			vus <- vu
		}(vu)
	}
	return nil
}
