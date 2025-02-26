// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package aggregator

import (
	"sync"
	"time"

	"github.com/DataDog/datadog-agent/pkg/collector/check"
	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/metrics"

	agentruntime "github.com/DataDog/datadog-agent/pkg/runtime"
	"github.com/DataDog/datadog-agent/pkg/serializer"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// DemultiplexerInstance is a shared global demultiplexer instance.
// Initialized by InitAndStartAgentDemultiplexer or InitAndStartServerlessDemultiplexer,
// could be nil otherwise.
//
// The plan is to deprecated this global instance at some point.
var demultiplexerInstance Demultiplexer

var demultiplexerInstanceMu sync.Mutex

// Demultiplexer is composed of multiple samplers (check and time/dogstatsd)
// a shared forwarder, the event platform forwarder, orchestrator data buffers
// and other data that need to be sent to the forwarders.
// AgentDemultiplexerOptions let you configure which forwarders have to be started.
type Demultiplexer interface {
	// General
	// --

	// Run runs all demultiplexer parts
	Run()
	// Stop stops the demultiplexer.
	// Resources are released, the instance should not be used after a call to `Stop()`.
	Stop(flush bool)
	// Serializer returns the serializer used by the Demultiplexer instance.
	Serializer() serializer.MetricSerializer

	// Aggregation API
	// --

	// AddTimeSample sends a MetricSample to the time sampler.
	// In sharded implementation, the metric is sent to the first time sampler.
	AddTimeSample(sample metrics.MetricSample)
	// AddTimeSampleBatch sends a batch of MetricSample to the given time
	// sampler shard.
	// Implementation not supporting sharding may ignore the `shard` parameter.
	AddTimeSampleBatch(shard TimeSamplerID, samples metrics.MetricSampleBatch)

	// AddLateMetrics pushes metrics in the no-aggregation pipeline: a pipeline
	// where the metrics are not sampled and sent as-is.
	// This is the method to use to send metrics with a valid timestamp attached.
	AddLateMetrics(metrics metrics.MetricSampleBatch)

	// ForceFlushToSerializer flushes all the aggregated data from the different samplers to
	// the serialization/forwarding parts.
	ForceFlushToSerializer(start time.Time, waitForSerializer bool)
	// GetMetricSamplePool returns a shared resource used in the whole DogStatsD
	// pipeline to re-use metric samples slices: the server is getting a slice
	// and filling it with samples, the rest of the pipeline process them the
	// end of line (the time sampler) is putting back the slice in the pool.
	// Main idea is to reduce the garbage generated by slices allocation.
	GetMetricSamplePool() *metrics.MetricSamplePool

	// Senders API, mainly used by collectors/checks
	// --

	GetSender(id check.ID) (Sender, error)
	SetSender(sender Sender, id check.ID) error
	DestroySender(id check.ID)
	GetDefaultSender() (Sender, error)
	ChangeAllSendersDefaultHostname(hostname string)
	cleanSenders()
}

// trigger be used to trigger something in the TimeSampler or the BufferedAggregator.
// If `blockChan` is not nil, a message is expected on this chan when the action is done.
// See `flushTrigger` to see the usage in a flush trigger.
type trigger struct {
	time time.Time

	// if not nil, the flusher will send a message in this chan when the flush is complete.
	blockChan chan struct{}

	// used by the BufferedAggregator to know if serialization of events,
	// service checks and such have to be waited for before returning
	// from Flush()
	waitForSerializer bool
}

// flushTrigger is a trigger used to flush data, results is expected to be written
// in flushedSeries (or seriesSink depending on the implementation) and flushedSketches.
type flushTrigger struct {
	trigger

	sketchesSink metrics.SketchesSink
	seriesSink   metrics.SerieSink
}

func createIterableMetrics(
	flushAndSerializeInParallel FlushAndSerializeInParallel,
	serializer serializer.MetricSerializer,
	logPayloads bool,
	isServerless bool,
) (*metrics.IterableSeries, *metrics.IterableSketches) {
	var series *metrics.IterableSeries
	var sketches *metrics.IterableSketches

	if serializer.AreSeriesEnabled() {
		series = metrics.NewIterableSeries(func(se *metrics.Serie) {
			if logPayloads {
				log.Debugf("Flushing serie: %s", se)
			}
			tagsetTlm.updateHugeSerieTelemetry(se)
		}, flushAndSerializeInParallel.BufferSize, flushAndSerializeInParallel.ChannelSize)
	}

	if serializer.AreSketchesEnabled() {
		sketches = metrics.NewIterableSketches(func(sketch *metrics.SketchSeries) {
			if logPayloads {
				log.Debugf("Flushing Sketches: %v", sketch)
			}
			if isServerless {
				log.DebugfServerless("Sending sketches payload : %s", sketch.String())
			}
			tagsetTlm.updateHugeSketchesTelemetry(sketch)
		}, flushAndSerializeInParallel.BufferSize, flushAndSerializeInParallel.ChannelSize)
	}
	return series, sketches
}

// sendIterableSeries is continuously sending series to the serializer, until another routine calls SenderStopped on the
// series sink.
// Mainly meant to be executed in its own routine, sendIterableSeries is closing the `done` channel once it has returned
// from SendIterableSeries (because the SenderStopped methods has been called on the sink).
func sendIterableSeries(serializer serializer.MetricSerializer, start time.Time, serieSource metrics.SerieSource) {
	log.Debug("Demultiplexer: sendIterableSeries: start sending iterable series to the serializer")
	err := serializer.SendIterableSeries(serieSource)
	// if err == nil, SenderStopped was called and it is safe to read the number of series.
	count := serieSource.Count()
	addFlushCount("Series", int64(count))
	updateSerieTelemetry(start, count, err)
	log.Debug("Demultiplexer: sendIterableSeries: stop routine")
}

// GetDogStatsDWorkerAndPipelineCount returns how many routines should be spawned
// for the DogStatsD workers and how many DogStatsD pipeline should be running.
func GetDogStatsDWorkerAndPipelineCount() (int, int) {
	return getDogStatsDWorkerAndPipelineCount(agentruntime.NumVCPU())
}

func getDogStatsDWorkerAndPipelineCount(vCPUs int) (int, int) {
	var dsdWorkerCount int
	var pipelineCount int
	autoAdjust := config.Datadog.GetBool("dogstatsd_pipeline_autoadjust")

	// no auto-adjust of the pipeline count:
	// we use the pipeline count configuration
	// to determine how many workers should be running
	// ------------------------------------

	if !autoAdjust {
		pipelineCount = config.Datadog.GetInt("dogstatsd_pipeline_count")
		if pipelineCount <= 0 { // guard against configuration mistakes
			pipelineCount = 1
		}

		// - a core for the listener goroutine
		// - one per aggregation pipeline (time sampler)
		// - the rest for workers
		// But we want at minimum 2 workers.
		dsdWorkerCount = vCPUs - 1 - pipelineCount

		if dsdWorkerCount < 2 {
			dsdWorkerCount = 2
		}

		return dsdWorkerCount, pipelineCount
	}

	// we will auto-adjust the pipeline and workers count
	//
	// Benchmarks have revealed that 3 very busy workers can be processed
	// by 2 pipelines DogStatsD and have a good ratio execution / scheduling / waiting.
	// To keep this simple for now, we will try running 1 less pipeline than workers.
	// (e.g. for 4 workers, 3 pipelines)
	// Use Go routines analysis with pprof to look at execution time if you want
	// adapt this heuristic.
	//
	// Basically the formula is:
	//  - half the amount of vCPUS for the amount of workers routines
	//  - half the amount of vCPUS - 1 for the amount of pipeline routines
	//  - this last routine for the listener routine

	dsdWorkerCount = vCPUs / 2
	if dsdWorkerCount < 2 { // minimum 2 workers
		dsdWorkerCount = 2
	}

	pipelineCount = dsdWorkerCount - 1
	if pipelineCount <= 0 { // minimum 1 pipeline
		pipelineCount = 1
	}

	if config.Datadog.GetInt("dogstatsd_pipeline_count") > 1 {
		log.Warn("DogStatsD pipeline count value ignored since 'dogstatsd_pipeline_autoadjust' is enabled.")
	}

	return dsdWorkerCount, pipelineCount
}
