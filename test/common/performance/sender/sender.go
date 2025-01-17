/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sender

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/golang/protobuf/ptypes/timestamp"
	"knative.dev/eventing/test/common/performance/common"
	pb "knative.dev/eventing/test/common/performance/event_state"
	pkgtest "knative.dev/pkg/test"
)

const (
	defaultEventSource   = "perf-test-event-source"
	warmupRps            = 100
	defaultTestNamespace = "default"
	podNameEnvVar        = "POD_NAME"
	podNamespaceEnvVar   = "POD_NAMESPACE"

	// Those two depends on the maximum tolerated latency. If latency is higher than 1 sec, increase these.
	// But if latency is higher than 1 sec, something else is wrong

	waitAfterWarmup = 5 * time.Second
)

type Sender struct {
	paceSpecs     []common.PaceSpec
	msgSize       uint
	warmupSeconds uint

	// EventTimestamp channels
	sentCh     chan common.EventTimestamp
	acceptedCh chan common.EventTimestamp
	failedCh   chan common.EventTimestamp

	// events recording maps
	sentEvents     *pb.EventsRecord
	acceptedEvents *pb.EventsRecord
	failedEvents   *pb.EventsRecord

	// load generator
	loadGenerator LoadGenerator

	// aggregator GRPC client
	aggregatorClient *pb.AggregatorClient
}

func NewSender(loadGeneratorFactory LoadGeneratorFactory, aggregAddr string, msgSize uint, warmupSeconds uint, paceFlag string) (common.Executor, error) {
	pacerSpecs, err := common.ParsePaceSpec(paceFlag)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse pace spec: %v", err)
	}

	// wait until all pods are ready (channel, consumers) to ensure we don’t start sending events too early
	// and the GRPC client can connect to the aggregator
	ns := testNamespace()
	log.Printf("Waiting for all Pods to be ready in namespace %s", ns)
	if err := waitForPods(ns); err != nil {
		return nil, fmt.Errorf("Timeout waiting for Pods readiness in namespace %s: %v", ns, err)
	}

	// create a connection to the aggregator
	aggregatorClient, err := pb.NewAggregatorClient(aggregAddr)
	if err != nil {
		return nil, fmt.Errorf("Failed to connect to the aggregator: %v", err)
	}

	// We need those estimates to allocate memory before benchmark starts
	estimatedNumberOfMessagesInsideAChannel, estimatedNumberOfTotalMessages := common.CalculateMemoryConstraintsForPaceSpecs(pacerSpecs)

	// Small note: receivedCh depends on receive thpt and not send thpt but we
	// don't care since this is a pessimistic estimate and receive thpt < send thpt
	// PS after 3 weeks: Yeah I know this is not an entirely true assumption after the system becomes
	// unstable, but we are interested to understand when the system becomes unstable,
	// not what happens after

	executor := &Sender{
		msgSize:       msgSize,
		warmupSeconds: warmupSeconds,
		paceSpecs:     pacerSpecs,

		sentCh:     make(chan common.EventTimestamp, estimatedNumberOfMessagesInsideAChannel),
		acceptedCh: make(chan common.EventTimestamp, estimatedNumberOfMessagesInsideAChannel),
		failedCh:   make(chan common.EventTimestamp, estimatedNumberOfMessagesInsideAChannel),

		sentEvents: &pb.EventsRecord{
			Type:   pb.EventsRecord_SENT,
			Events: make(map[string]*timestamp.Timestamp, estimatedNumberOfTotalMessages),
		},
		acceptedEvents: &pb.EventsRecord{
			Type:   pb.EventsRecord_ACCEPTED,
			Events: make(map[string]*timestamp.Timestamp, estimatedNumberOfTotalMessages),
		},
		failedEvents: &pb.EventsRecord{
			Type:   pb.EventsRecord_FAILED,
			Events: make(map[string]*timestamp.Timestamp, estimatedNumberOfTotalMessages),
		},

		aggregatorClient: aggregatorClient,
	}

	executor.loadGenerator, err = loadGeneratorFactory(eventsSource(), executor.sentCh, executor.acceptedCh, executor.failedCh)
	if err != nil {
		return nil, err
	}

	return executor, nil
}

func (s *Sender) Run(ctx context.Context) {
	// --- Warmup phase
	log.Printf("--- BEGIN WARMUP ---")
	if s.warmupSeconds > 0 {
		if err := s.warmup(ctx, s.warmupSeconds); err != nil {
			log.Fatalf("Failed to run warmup: %v", err)
		}
	} else {
		log.Printf("Warmup skipped")
	}
	log.Printf("---- END WARMUP ----")

	log.Printf("--- BEGIN BENCHMARK ---")

	// Start the events processor
	log.Println("Starting events processor")
	go s.processEvents()

	// Clean mess before starting
	runtime.GC()

	log.Println("Starting benchmark")

	// Run all pace configurations
	benchmarkBeginning := time.Now()
	for i, pace := range s.paceSpecs {
		log.Printf("Starting pace %d° at %v rps for %v seconds", i+1, pace.Rps, pace.Duration)
		s.loadGenerator.RunPace(i, pace, s.msgSize)

		// Wait for flush
		time.Sleep(common.WaitForFlush)

		// Trigger GC
		log.Println("Triggering GC")
		s.loadGenerator.SendGCEvent()
		runtime.GC()

		// Wait for receivers GC
		time.Sleep(common.WaitForReceiverGC)
	}

	s.loadGenerator.SendEndEvent()

	log.Printf("Benchmark completed in %v", time.Since(benchmarkBeginning))

	s.closeChannels()

	log.Println("---- END BENCHMARK ----")

	log.Println("Sending collected data to the aggregator")

	log.Printf("%-15s: %d", "Sent count", len(s.sentEvents.Events))
	log.Printf("%-15s: %d", "Accepted count", len(s.acceptedEvents.Events))
	log.Printf("%-15s: %d", "Failed count", len(s.failedEvents.Events))

	err := s.aggregatorClient.Publish(&pb.EventsRecordList{Items: []*pb.EventsRecord{
		s.sentEvents,
		s.acceptedEvents,
		s.failedEvents,
	}})
	if err != nil {
		log.Fatalf("Failed to send events record: %v\n", err)
	}
}

func (s *Sender) warmup(ctx context.Context, warmupSeconds uint) error {
	log.Println("Starting warmup")

	s.loadGenerator.Warmup(common.PaceSpec{Rps: warmupRps, Duration: time.Duration(warmupSeconds) * time.Second}, s.msgSize)

	// give the channel some time to drain the events it may still have enqueued
	time.Sleep(waitAfterWarmup)

	return nil
}

func (s *Sender) closeChannels() {
	log.Printf("All requests sent")

	close(s.sentCh)
	close(s.acceptedCh)
	close(s.failedCh)

	log.Printf("All channels closed")
}

// processEvents keeps a record of all events (sent, accepted, failed, received).
func (s *Sender) processEvents() {
	for {
		select {
		case e, ok := <-s.sentCh:
			if !ok {
				continue
			}
			s.sentEvents.Events[e.EventId] = e.At

		case e, ok := <-s.acceptedCh:
			if !ok {
				continue
			}
			s.acceptedEvents.Events[e.EventId] = e.At

		case e, ok := <-s.failedCh:
			if !ok {
				continue
			}
			s.failedEvents.Events[e.EventId] = e.At
		}
	}
}

func eventsSource() string {
	if pn := os.Getenv(podNameEnvVar); pn != "" {
		return pn
	}
	return defaultEventSource
}

func testNamespace() string {
	if pn := os.Getenv(podNamespaceEnvVar); pn != "" {
		return pn
	}
	return defaultTestNamespace
}

func waitForPods(namespace string) error {
	c, err := pkgtest.NewKubeClient("", "")
	if err != nil {
		return err
	}
	return pkgtest.WaitForAllPodsRunning(c, namespace)
}
