/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aggregator

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/google/mako/go/quickstore"

	"google.golang.org/grpc"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"

	"knative.dev/eventing/test/common/performance/common"
	pb "knative.dev/eventing/test/common/performance/event_state"
	"knative.dev/pkg/test/mako"
)

const maxRcvMsgSize = 1024 * 1024 * 100

// thread-safe events recording map
type eventsRecord struct {
	sync.RWMutex
	*pb.EventsRecord
}

var fatalf = log.Fatalf

type Aggregator struct {
	// thread-safe events recording maps
	sentEvents     *eventsRecord
	acceptedEvents *eventsRecord
	failedEvents   *eventsRecord
	receivedEvents *eventsRecord

	// channel to notify the main goroutine that an events record has been received
	notifyEventsReceived chan struct{}

	// GRPC server
	listener net.Listener
	server   *grpc.Server

	makoTags      []string
	expectRecords uint

	benchmarkKey  string
	benchmarkName string
}

func NewAggregator(benchmarkKey, benchmarkName, listenAddr string, expectRecords uint, makoTags []string) (common.Executor, error) {
	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create listener: %v", err)
	}

	executor := &Aggregator{
		listener:             l,
		notifyEventsReceived: make(chan struct{}),
		makoTags:             makoTags,
		expectRecords:        expectRecords,
		benchmarkKey:         benchmarkKey,
		benchmarkName:        benchmarkName,
	}

	// --- Create GRPC server

	s := grpc.NewServer(grpc.MaxRecvMsgSize(maxRcvMsgSize))
	pb.RegisterEventsRecorderServer(s, executor)
	executor.server = s

	// --- Initialize records maps

	executor.sentEvents = &eventsRecord{EventsRecord: &pb.EventsRecord{
		Type:   pb.EventsRecord_SENT,
		Events: make(map[string]*timestamp.Timestamp),
	}}
	executor.acceptedEvents = &eventsRecord{EventsRecord: &pb.EventsRecord{
		Type:   pb.EventsRecord_ACCEPTED,
		Events: make(map[string]*timestamp.Timestamp),
	}}
	executor.failedEvents = &eventsRecord{EventsRecord: &pb.EventsRecord{
		Type:   pb.EventsRecord_FAILED,
		Events: make(map[string]*timestamp.Timestamp),
	}}
	executor.receivedEvents = &eventsRecord{EventsRecord: &pb.EventsRecord{
		Type:   pb.EventsRecord_RECEIVED,
		Events: make(map[string]*timestamp.Timestamp),
	}}

	return executor, nil
}

func (ag *Aggregator) Run(ctx context.Context) {
	log.Printf("Configuring Mako")

	// Use the benchmark key created
	// TODO support to check benchmark key for dev or prod
	client, err := mako.SetupWithBenchmarkConfig(ctx, &ag.benchmarkKey, &ag.benchmarkName, ag.makoTags...)
	if err != nil {
		fatalf("Failed to setup mako: %v", err)
	}

	// Use a fresh context here so that our RPC to terminate the sidecar
	// isn't subject to our timeout (or we won't shut it down when we time out)
	defer client.ShutDownFunc(context.Background())

	// Wrap fatalf in a helper or our sidecar will live forever.
	fatalf = func(f string, args ...interface{}) {
		client.ShutDownFunc(context.Background())
		log.Fatalf(f, args...)
	}

	// --- Run GRPC events receiver
	log.Printf("Starting events recorder server")

	go func() {
		if err := ag.server.Serve(ag.listener); err != nil {
			fatalf("Failed to serve: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		log.Printf("Terminating events recorder server")
		ag.server.GracefulStop()
	}()

	// --- Wait for all records
	log.Printf("Expecting %d events records", ag.expectRecords)
	ag.waitForEvents()
	log.Printf("Received all expected events records")

	ag.server.GracefulStop()

	// --- Publish latencies
	log.Printf("%-15s: %d", "Sent count", len(ag.sentEvents.Events))
	log.Printf("%-15s: %d", "Accepted count", len(ag.acceptedEvents.Events))
	log.Printf("%-15s: %d", "Failed count", len(ag.failedEvents.Events))
	log.Printf("%-15s: %d", "Received count", len(ag.receivedEvents.Events))

	log.Printf("Publishing latencies")

	// count errors
	var publishErrorCount int
	var deliverErrorCount int

	for sentID := range ag.sentEvents.Events {
		timestampSentProto := ag.sentEvents.Events[sentID]
		timestampSent, _ := ptypes.Timestamp(timestampSentProto)

		timestampAcceptedProto, accepted := ag.acceptedEvents.Events[sentID]
		timestampAccepted, _ := ptypes.Timestamp(timestampAcceptedProto)

		timestampReceivedProto, received := ag.receivedEvents.Events[sentID]
		timestampReceived, _ := ptypes.Timestamp(timestampReceivedProto)

		if !accepted {
			errMsg := "Failed on broker"
			if _, failed := ag.failedEvents.Events[sentID]; !failed {
				errMsg = "Event not accepted but missing from failed map"
			}

			deliverErrorCount++

			if qerr := client.Quickstore.AddError(mako.XTime(timestampSent), errMsg); qerr != nil {
				log.Printf("ERROR AddError: %v", qerr)
			}
			continue
		}

		sendLatency := timestampAccepted.Sub(timestampSent)
		// Uncomment to get CSV directly from this container log
		//fmt.Printf("%f,%d,\n", mako.XTime(timestampSent), sendLatency.Nanoseconds())
		// TODO mako accepts float64, which imo could lead to losing some precision on local tests. It should accept int64
		if qerr := client.Quickstore.AddSamplePoint(mako.XTime(timestampSent), map[string]float64{"pl": sendLatency.Seconds()}); qerr != nil {
			log.Printf("ERROR AddSamplePoint: %v", qerr)
		}

		if !received {
			publishErrorCount++

			if qerr := client.Quickstore.AddError(mako.XTime(timestampSent), "Event not delivered"); qerr != nil {
				log.Printf("ERROR AddError: %v", qerr)
			}
			continue
		}

		e2eLatency := timestampReceived.Sub(timestampSent)
		// Uncomment to get CSV directly from this container log
		//fmt.Printf("%f,,%d\n", mako.XTime(timestampSent), e2eLatency.Nanoseconds())
		// TODO mako accepts float64, which imo could lead to losing some precision on local tests. It should accept int64
		if qerr := client.Quickstore.AddSamplePoint(mako.XTime(timestampSent), map[string]float64{"dl": e2eLatency.Seconds()}); qerr != nil {
			log.Printf("ERROR AddSamplePoint: %v", qerr)
		}
	}

	// --- Publish throughput

	log.Printf("Publishing throughputs")

	sentTimestamps := eventsToTimestampsArray(&ag.sentEvents.Events)
	err = publishThpt(sentTimestamps, client.Quickstore, "st")
	if err != nil {
		log.Printf("ERROR AddSamplePoint: %v", err)
	}

	receivedTimestamps := eventsToTimestampsArray(&ag.receivedEvents.Events)
	err = publishThpt(receivedTimestamps, client.Quickstore, "dt")
	if err != nil {
		log.Printf("ERROR AddSamplePoint: %v", err)
	}

	failureTimestamps := eventsToTimestampsArray(&ag.failedEvents.Events)
	if len(failureTimestamps) > 2 {
		err = publishThpt(failureTimestamps, client.Quickstore, "ft")
		if err != nil {
			log.Printf("ERROR AddSamplePoint: %v", err)
		}
	}

	// --- Publish error counts as aggregate metrics

	log.Printf("Publishing aggregates")

	client.Quickstore.AddRunAggregate("pe", float64(publishErrorCount))
	client.Quickstore.AddRunAggregate("de", float64(deliverErrorCount))

	log.Printf("Store to mako")

	if out, err := client.Quickstore.Store(); err != nil {
		fatalf("Failed to store data: %v\noutput: %v", err, out)
	}

	log.Printf("Aggregation completed")
}

func eventsToTimestampsArray(events *map[string]*timestamp.Timestamp) []time.Time {
	values := make([]time.Time, 0, len(*events))
	for _, v := range *events {
		t, _ := ptypes.Timestamp(v)
		values = append(values, t)
	}
	sort.Slice(values, func(x, y int) bool { return values[x].Before(values[y]) })
	return values
}

func publishThpt(timestamps []time.Time, q *quickstore.Quickstore, metricName string) error {
	for i, t := range timestamps[1:] {
		var thpt uint
		j := i - 1
		for j >= 0 && t.Sub(timestamps[j]) <= time.Second {
			thpt++
			j--
		}
		if qerr := q.AddSamplePoint(mako.XTime(t), map[string]float64{metricName: float64(thpt)}); qerr != nil {
			return qerr
		}
	}
	return nil
}

// waitForEvents blocks until the expected number of events records has been received.
func (ag *Aggregator) waitForEvents() {
	for receivedRecords := uint(0); receivedRecords < ag.expectRecords; receivedRecords++ {
		<-ag.notifyEventsReceived
	}
}

// RecordSentEvents implements event_state.EventsRecorder
func (ag *Aggregator) RecordEvents(_ context.Context, in *pb.EventsRecordList) (*pb.RecordReply, error) {
	defer func() {
		ag.notifyEventsReceived <- struct{}{}
	}()

	for _, recIn := range in.Items {
		recType := recIn.GetType()

		var rec *eventsRecord

		switch recType {
		case pb.EventsRecord_SENT:
			rec = ag.sentEvents
		case pb.EventsRecord_ACCEPTED:
			rec = ag.acceptedEvents
		case pb.EventsRecord_FAILED:
			rec = ag.failedEvents
		case pb.EventsRecord_RECEIVED:
			rec = ag.receivedEvents
		default:
			log.Printf("Ignoring events record of type %s", recType)
			continue
		}

		log.Printf("-> Recording %d %s events", uint64(len(recIn.Events)), recType)

		func() {
			rec.Lock()
			defer rec.Unlock()
			for id, t := range recIn.Events {
				if _, exists := rec.Events[id]; exists {
					log.Printf("!! Found duplicate %s event ID %s", recType, id)
					continue
				}
				rec.Events[id] = t
			}
		}()
	}

	return &pb.RecordReply{Count: uint32(len(in.Items))}, nil
}
