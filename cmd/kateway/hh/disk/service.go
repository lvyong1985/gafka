package disk

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/funkygao/golib/timewheel"
	log "github.com/funkygao/log4go"
)

type Service struct {
	cfg *Config

	closed bool

	rwmux sync.RWMutex

	// hh
	// ├── cluster1
	// └── cluster2
	//     ├── topic1
	//     └── topic2
	//         ├── 1
	//         ├── 2
	//         ├── 3
	//         └── cursor.dmp
	queues map[clusterTopic]*queue
}

func New(cfg *Config) *Service {
	timer = timewheel.NewTimeWheel(time.Second, 120)
	return &Service{
		cfg:    cfg,
		queues: make(map[clusterTopic]*queue),
		closed: true,
	}
}

func (this *Service) Name() string {
	return "disk"
}

func (this *Service) Start() (err error) {
	for _, dir := range this.cfg.Dirs {
		if err = mkdirIfNotExist(dir); err != nil {
			return
		}

		if err = this.loadQueues(dir, true); err != nil {
			return
		}

	}

	this.closed = false
	return
}

func (this *Service) Stop() {
	this.rwmux.Lock()
	defer this.rwmux.Unlock()

	if this.closed {
		return
	}

	for _, q := range this.queues {
		q.Close()
	}
	this.queues = make(map[clusterTopic]*queue)

	timer.Stop()
	this.closed = true
}

func (this *Service) Inflights() (n int64) {
	this.rwmux.RLock()
	for _, q := range this.queues {
		n += q.Inflights()
	}
	this.rwmux.RUnlock()
	return
}

func (this *Service) Append(cluster, topic string, key, value []byte) error {
	if this.closed {
		return ErrNotOpen
	}

	b := &block{magic: currentMagic, key: key, value: value}
	ct := clusterTopic{cluster: cluster, topic: topic}

	this.rwmux.RLock()
	q, present := this.queues[ct]
	this.rwmux.RUnlock()
	if present {
		return q.Append(b)
	}

	this.rwmux.Lock()
	defer this.rwmux.Unlock()

	// double lock check
	q, present = this.queues[ct]
	if present {
		return q.Append(b)
	}

	if err := this.createAndOpenQueue(ct, true); err != nil {
		return err
	}

	return this.queues[ct].Append(b)
}

func (this *Service) Empty(cluster, topic string) bool {
	ct := clusterTopic{cluster: cluster, topic: topic}

	this.rwmux.RLock()
	q, present := this.queues[ct]
	this.rwmux.RUnlock()

	if !present {
		// should never happen
		return true
	}

	return q.EmptyInflight()
}

func (this *Service) FlushInflights() {
	if !this.closed {
		// will race with queue housekeeping
		log.Error("hh[%s] run flush inflights with service closed!", this.Name())
		return
	}

	for _, dir := range this.cfg.Dirs {
		if err := this.loadQueues(dir, false); err != nil {
			log.Error("hh[%s] flush inflights %s: %s", this.Name(), dir, err)
			return
		}
	}

	var (
		queueWg, errWg sync.WaitGroup
		failCh         = make(chan error, len(this.queues))
	)
	for _, q := range this.queues {
		queueWg.Add(1)
		go q.FlushInflights(failCh, &queueWg)
	}

	errWg.Add(1)
	go func() {
		for err := range failCh {
			log.Error("hh[%s] flush inflights: %s", this.Name(), err)
		}

		errWg.Done()
	}()

	queueWg.Wait()
	close(failCh)
	errWg.Wait()
}

func (this *Service) loadQueues(dir string, startQueues bool) error {
	clusters, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}

	// load queues from disk
	for _, cluster := range clusters {
		if !cluster.IsDir() {
			continue
		}

		topics, err := ioutil.ReadDir(filepath.Join(dir, cluster.Name()))
		if err != nil {
			return err
		}

		for _, topic := range topics {
			if !topic.IsDir() {
				continue
			}

			ct := clusterTopic{cluster: cluster.Name(), topic: topic.Name()}
			if err = this.createAndOpenQueue(ct, startQueues); err != nil {
				return err
			}
		}
	}

	return nil
}

func (this *Service) createAndOpenQueue(ct clusterTopic, start bool) error {
	dir := this.nextDir()

	if err := os.MkdirAll(ct.ClusterDir(dir), 0700); err != nil && !os.IsExist(err) {
		return err
	}

	this.queues[ct] = newQueue(ct, ct.TopicDir(dir), -1, this.cfg.PurgeInterval, this.cfg.MaxAge)
	if err := this.queues[ct].Open(); err != nil {
		return err
	}
	if start {
		this.queues[ct].Start()
	}

	return nil
}

func (this *Service) nextDir() string {
	// find least loaded dir
	return this.cfg.Dirs[0] // TODO
}
