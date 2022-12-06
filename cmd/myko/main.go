package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/mykodev/myko/datastore/cassandra"
	pb "github.com/mykodev/myko/proto"
)

var (
	listen           string
	database         string // comma separated list of peers
	databaseUser     string
	databasePassword string
	datacenter       string
	timeout          time.Duration
	flushDuration    time.Duration
)

func main() {
	flag.StringVar(&listen, "listen", ":6959", "")
	flag.StringVar(&database, "cassandra", "localhost:9043", "")
	flag.StringVar(&databaseUser, "cassandra-user", "", "")
	flag.StringVar(&databasePassword, "cassandra-passwd", "", "")
	flag.StringVar(&datacenter, "datacenter", "", "")
	flag.DurationVar(&timeout, "timeout", 10*time.Second, "")
	flag.DurationVar(&flushDuration, "flush-every", 5*time.Second, "")
	flag.Parse()

	session, err := cassandra.NewSession(cassandra.Options{
		Peers:          strings.Split(database, ","),
		User:           databaseUser,
		Password:       databasePassword,
		Datacenter:     datacenter,
		DefaultTimeout: timeout,
	})
	if err != nil {
		log.Fatalf("Failed to create a connection to datastore: %v", err)
	}

	log.Printf("Starting the myko server at %q...", listen)
	server := pb.NewServiceServer(
		&service{
			session:     session,
			batchWriter: newBatchWriter(session, 100),
		}, nil)
	log.Fatal(http.ListenAndServe(listen, server))
}

type service struct {
	session     *gocql.Session
	batchWriter *batchWriter
}

func (s *service) Query(ctx context.Context, req *pb.QueryRequest) (*pb.QueryResponse, error) {
	filter := cassandra.Filter{
		TraceID: req.TraceId,
		Origin:  req.Origin,
		Event:   req.Event,
	}
	filterCQL, err := filter.CQL()
	if err != nil {
		return nil, err
	}

	iter := s.session.Query(`
		SELECT origin, event, value, unit
		FROM events.data
		` + filterCQL + ` ALLOW FILTERING`).Iter()

	var (
		origin string
		name   string
		unit   string
		value  float64
	)

	key := func(origin, event, unit string) string {
		return origin + ":" + event + ":" + unit
	}

	v := make(map[string]*pb.Event)
	for iter.Scan(&origin, &name, &value, &unit) {
		k := key(origin, name, unit)
		event, ok := v[k]
		if ok {
			event.Value += value
			v[k] = event
		} else {
			v[k] = &pb.Event{
				Name:  name,
				Value: value,
				Unit:  unit,
			}
		}
	}
	var events []*pb.Event
	for _, e := range v {
		events = append(events, &pb.Event{
			Name:  e.Name,
			Unit:  e.Unit,
			Value: e.Value,
		})
	}

	sorter := &eventSorter{events: events}
	sort.Sort(sorter)
	return &pb.QueryResponse{Events: sorter.events}, nil
}

func (s *service) InsertEvents(ctx context.Context, req *pb.InsertEventsRequest) (*pb.InsertEventsResponse, error) {
	for _, entry := range req.Entries {
		if err := s.batchWriter.Write(entry); err != nil {
			return nil, err
		}
	}
	return &pb.InsertEventsResponse{}, nil
}

func (s *service) DeleteEvents(ctx context.Context, req *pb.DeleteEventsRequest) (*pb.DeleteEventsResponse, error) {
	filter := cassandra.Filter{
		TraceID: req.TraceId,
		Origin:  req.Origin,
		Event:   req.Event,
	}
	filterCQL, err := filter.CQL()
	if err != nil {
		return nil, err
	}

	iter := s.session.Query(`SELECT id FROM events.data ` +
		filterCQL + ` ALLOW FILTERING`).Iter()
	var id gocql.UUID
	for iter.Scan(&id) {
		log.Printf("Deleting %q", id)
		if err := s.session.Query(`DELETE FROM events.data WHERE id = ?`, id.String()).Exec(); err != nil {
			return nil, err
		}
	}
	return &pb.DeleteEventsResponse{}, nil
}

func newBatchWriter(session *gocql.Session, n int) *batchWriter {
	// TODO: Implement an optional WAL.
	return &batchWriter{
		n:       n,
		session: session,
		events:  make(map[string]*pb.Event, n),
	}
}

type batchWriter struct {
	mu         sync.Mutex
	events     map[string]*pb.Event
	lastExport time.Time

	n       int
	session *gocql.Session
}

func (b *batchWriter) key(origin, traceID, name, unit string) string {
	return origin + ":" + traceID + ":" + name + ":" + unit
}

func (b *batchWriter) parseKey(key string) (origin, traceID, name, unit string) {
	v := strings.Split(key, ":")
	return v[0], v[1], v[2], v[3]
}

func (b *batchWriter) Write(e *pb.Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, event := range e.Events {
		key := b.key(e.Origin, e.TraceId, event.Name, event.Unit)
		v, ok := b.events[key]
		if !ok {
			b.events[key] = event
		} else {
			v.Value += event.Value
			b.events[key] = v
		}
	}
	return b.flushIfNeeded()
}

func (b *batchWriter) flushIfNeeded() error {
	// flushIfNeeded need to be called from Write.
	if len(b.events) > b.n || b.lastExport.Before(time.Now().Add(-1*flushDuration)) {
		log.Printf("Batch writing %d records", len(b.events))
		batch := b.session.NewBatch(gocql.LoggedBatch)
		for key, e := range b.events {
			origin, traceID, name, unit := b.parseKey(key)

			id, err := gocql.RandomUUID()
			if err != nil {
				return err
			}
			batch.Query(`
				INSERT INTO events.data 
				(id, trace_id, origin, event, value, unit, created_at)
				VALUES ( ?, ?, ?, ?, ?, ?, ? )`,
				id.String(), origin, traceID, name, e.Value, unit, time.Now())
		}
		if err := b.session.ExecuteBatch(batch); err != nil {
			return err
		}
		b.events = make(map[string]*pb.Event, b.n)
		b.lastExport = time.Now()
	}
	return nil
}

type eventSorter struct {
	events []*pb.Event
}

func (s *eventSorter) Len() int {
	return len(s.events)
}

func (s *eventSorter) Less(i, j int) bool {
	return s.events[i].Name < s.events[j].Name
}

func (s *eventSorter) Swap(i, j int) {
	s.events[i], s.events[j] = s.events[j], s.events[i]
}
