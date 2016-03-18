package minion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dnaeon/backoff"
	"github.com/dnaeon/gru/classifier"
	"github.com/dnaeon/gru/task"
	"github.com/dnaeon/gru/utils"

	etcdclient "github.com/coreos/etcd/client"
	"github.com/pborman/uuid"
	"golang.org/x/net/context"
)

// EtcdMinionSpace is the keyspace in etcd used by minions
const EtcdMinionSpace = "/gru/minion"

// Etcd Minion
type etcdMinion struct {
	// Name of the minion
	name string

	// Minion root directory in etcd
	rootDir string

	// Minion queue directory in etcd
	queueDir string

	// Log directory of previously executed tasks
	logDir string

	// Classifier directory in etcd
	classifierDir string

	// Minion unique identifier
	id uuid.UUID

	// KeysAPI client to etcd
	kapi etcdclient.KeysAPI

	// Channel over which tasks are sent for processing
	taskQueue chan *task.Task

	// Channel used to signal shutdown time
	done chan struct{}
}

// NewEtcdMinion creates a new minion with etcd backend
func NewEtcdMinion(name string, cfg etcdclient.Config) Minion {
	c, err := etcdclient.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	kapi := etcdclient.NewKeysAPI(c)
	id := utils.GenerateUUID(name)
	rootDir := filepath.Join(EtcdMinionSpace, id.String())
	queueDir := filepath.Join(rootDir, "queue")
	classifierDir := filepath.Join(rootDir, "classifier")
	logDir := filepath.Join(rootDir, "log")
	taskQueue := make(chan *task.Task)
	done := make(chan struct{})

	m := &etcdMinion{
		name:          name,
		rootDir:       rootDir,
		queueDir:      queueDir,
		classifierDir: classifierDir,
		logDir:        logDir,
		id:            id,
		kapi:          kapi,
		taskQueue:     taskQueue,
		done:          done,
	}

	return m
}

// Checks for any pending tasks and sends them
// for processing if there are any
func (m *etcdMinion) checkQueue() error {
	opts := &etcdclient.GetOptions{
		Recursive: true,
		Sort:      true,
	}

	// Get backlog tasks if any
	// If the directory key in etcd is missing that is okay, since
	// it means there are no pending tasks for processing
	resp, err := m.kapi.Get(context.Background(), m.queueDir, opts)
	if err != nil {
		if eerr, ok := err.(etcdclient.Error); !ok || eerr.Code == etcdclient.ErrorCodeKeyNotFound {
			return err
		}
	}

	backlog := resp.Node.Nodes
	if len(backlog) == 0 {
		// No backlog tasks found
		return nil
	}

	log.Printf("Found %d pending tasks in queue", len(backlog))
	for _, node := range backlog {
		t, err := EtcdUnmarshalTask(node)
		m.kapi.Delete(context.Background(), node.Key, nil)

		if err != nil {
			continue
		}

		m.taskQueue <- t
	}

	return nil
}

// Runs periodic jobs such as refreshing classifiers and
// updating the lastseen time every five minutes
func (m *etcdMinion) periodicRunner() {
	schedule := time.Minute * 5
	ticker := time.NewTicker(schedule)
	log.Printf("Periodic scheduler set to run every %s\n", schedule)

	for {
		select {
		case <-m.done:
			break
		case now := <-ticker.C:
			// Run periodic jobs
			m.classify()
			m.checkQueue()
			m.SetLastseen(now.Unix())
		}
	}
}

// Processes new tasks
func (m *etcdMinion) processTask(t *task.Task) error {
	// Update state of task to indicate that we are now processing it
	t.State = task.TaskStateProcessing
	m.SaveTaskResult(t)

	log.Printf("Processing task %s\n", t.TaskID)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Loaded %d resources from catalog", t.Catalog.Len())
	err := t.Catalog.Run(&buf)
	t.TimeProcessed = time.Now().Unix()
	t.Result = buf.String()

	if err != nil {
		log.Printf("Failed to process task %s: %s\n", t.TaskID, err)
		t.State = task.TaskStateFailed
	} else {
		log.Printf("Finished processing task %s\n", t.TaskID)
		t.State = task.TaskStateSuccess
	}

	m.SaveTaskResult(t)

	return err
}

// Classifies the minion
func (m *etcdMinion) classify() error {
	for key := range classifier.Registry {
		klassifier, err := classifier.Get(key)

		if err != nil {
			log.Printf("Failed to get classifier %s: %s\n", key, err)
			continue
		}

		m.SetClassifier(klassifier)
	}

	return nil
}

// EtcdUnmarshalTask unmarshals tasks from an etcd node
func EtcdUnmarshalTask(node *etcdclient.Node) (*task.Task, error) {
	task := new(task.Task)
	err := json.Unmarshal([]byte(node.Value), &task)

	return task, err
}

// ID returns the minion unique identifier
func (m *etcdMinion) ID() uuid.UUID {
	return m.id
}

// SetName sets the human-readable name of the minion in etcd
func (m *etcdMinion) SetName(name string) error {
	nameKey := filepath.Join(m.rootDir, "name")
	opts := &etcdclient.SetOptions{
		PrevExist: etcdclient.PrevIgnore,
	}

	_, err := m.kapi.Set(context.Background(), nameKey, name, opts)
	if err != nil {
		log.Printf("Failed to set name of minion: %s\n", err)
	}

	return err
}

// SetLastseen sets the time the minion was last seen in
// seconds since the Epoch
func (m *etcdMinion) SetLastseen(s int64) error {
	lastseenKey := filepath.Join(m.rootDir, "lastseen")
	lastseenValue := strconv.FormatInt(s, 10)
	opts := &etcdclient.SetOptions{
		PrevExist: etcdclient.PrevIgnore,
	}

	_, err := m.kapi.Set(context.Background(), lastseenKey, lastseenValue, opts)
	if err != nil {
		log.Printf("Failed to set lastseen time: %s\n", err)
	}

	return err
}

// SetClassifier sets a classifier for the minion in etcd
func (m *etcdMinion) SetClassifier(c *classifier.Classifier) error {
	// Classifiers in etcd expire after an hour
	opts := &etcdclient.SetOptions{
		PrevExist: etcdclient.PrevIgnore,
		TTL:       time.Hour,
	}

	// Serialize classifier to JSON and save it in etcd
	data, err := json.Marshal(c)
	if err != nil {
		log.Printf("Failed to serialize classifier %s: %s\n", c.Key, err)
		return err
	}

	// Classifier key in etcd
	klassifierKey := filepath.Join(m.classifierDir, c.Key)
	_, err = m.kapi.Set(context.Background(), klassifierKey, string(data), opts)

	if err != nil {
		log.Printf("Failed to set classifier %s: %s\n", c.Key, err)
		return err
	}

	return nil
}

// TaskListener monitors etcd for new tasks
func (m *etcdMinion) TaskListener(c chan<- *task.Task) error {
	log.Printf("Task listener is watching %s\n", m.queueDir)

	rand.Seed(time.Now().UTC().UnixNano())
	b := backoff.Backoff{
		Min:    1 * time.Second,
		Max:    10 * time.Minute,
		Factor: 2.0,
		Jitter: true,
	}

	watcherOpts := &etcdclient.WatcherOptions{
		Recursive: true,
	}
	watcher := m.kapi.Watcher(m.queueDir, watcherOpts)

	for {
		resp, err := watcher.Next(context.Background())
		if err != nil {
			// Use a backoff and retry later again
			duration := b.Duration()
			log.Printf("%s, retrying in %s\n", err, duration)
			time.Sleep(duration)
			continue
		}

		// Reset the backoff counter on successful receive
		b.Reset()

		// Ignore "delete" events when removing a task from the queue
		action := strings.ToLower(resp.Action)
		if strings.EqualFold(action, "delete") {
			continue
		}

		// Unmarshal and remove task from the queue
		t, err := EtcdUnmarshalTask(resp.Node)
		m.kapi.Delete(context.Background(), resp.Node.Key, nil)

		if err != nil {
			log.Printf("Received invalid task %s: %s\n", resp.Node.Key, err)
			continue
		}

		log.Printf("Received task %s\n", t.TaskID)
		c <- t
	}

	return nil
}

// TaskRunner processes new tasks
func (m *etcdMinion) TaskRunner(c <-chan *task.Task) error {
	log.Println("Starting task runner")

	for {
		select {
		case <-m.done:
			break
		case t := <-c:
			t.State = task.TaskStateQueued
			t.TimeReceived = time.Now().Unix()
			m.SaveTaskResult(t)

			if t.IsConcurrent {
				go m.processTask(t)
			} else {
				m.processTask(t)
			}
		}
	}

	return nil
}

// SaveTaskResult stores the result of a task in etcd
func (m *etcdMinion) SaveTaskResult(t *task.Task) error {
	taskKey := filepath.Join(m.logDir, t.TaskID.String())

	data, err := json.Marshal(t)
	if err != nil {
		log.Printf("Failed to serialize task %s: %s\n", t.TaskID, err)
		return err
	}

	opts := &etcdclient.SetOptions{
		PrevExist: etcdclient.PrevIgnore,
	}

	_, err = m.kapi.Set(context.Background(), taskKey, string(data), opts)
	if err != nil {
		log.Printf("Failed to save task %s: %s\n", t.TaskID, err)
	}

	return err
}

// Seve starts the minion
func (m *etcdMinion) Serve() error {
	err := m.SetName(m.name)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	err = m.SetLastseen(now)
	if err != nil {
		return err
	}

	// Start minion services
	go m.classify()
	go m.checkQueue()
	go m.periodicRunner()
	go m.TaskRunner(m.taskQueue)
	go m.TaskListener(m.taskQueue)

	log.Printf("Minion %s is ready to serve", m.ID())

	return nil
}

// Stop shutdowns the minions and its services
func (m *etcdMinion) Stop() error {
	log.Println("Minion is shutting down")

	close(m.taskQueue)
	close(m.done)

	return nil
}
