package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	_ "expvar"
	_ "net/http/pprof"

	"github.com/funkygao/gafka"
	"github.com/funkygao/gafka/cmd/kateway/meta"
	"github.com/funkygao/gafka/cmd/kateway/meta/zkmeta"
	"github.com/funkygao/gafka/cmd/kateway/store"
	"github.com/funkygao/gafka/cmd/kateway/store/dumb"
	"github.com/funkygao/gafka/cmd/kateway/store/kafka"
	"github.com/funkygao/gafka/ctx"
	"github.com/funkygao/gafka/registry"
	"github.com/funkygao/gafka/registry/zk"
	"github.com/funkygao/golib/ratelimiter"
	"github.com/funkygao/golib/signal"
	"github.com/funkygao/golib/timewheel"
	log "github.com/funkygao/log4go"
)

// Gateway is a distributed kafka Pub/Sub HTTP endpoint.
type Gateway struct {
	id        string
	startedAt time.Time
	zone      string

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	wg           sync.WaitGroup

	certFile string
	keyFile  string

	// online http producers client
	producers    map[string]Producer // key is remote addr
	produersLock sync.RWMutex

	// online http consumers client
	consumers     map[string]Consumer // key is remote addr
	consumersLock sync.RWMutex

	// cluster of kateway pub, only for client side load balance
	pubPeers     []string
	pubPeersLock sync.RWMutex

	pubServer *pubServer
	subServer *subServer
	manServer *manServer

	pubMetrics *pubMetrics
	subMetrics *subMetrics
	svrMetrics *serverMetrics

	guard        *guard
	timer        *timewheel.TimeWheel
	leakyBuckets *ratelimiter.LeakyBuckets // TODO
}

func NewGateway(id string, metaRefreshInterval time.Duration) *Gateway {
	this := &Gateway{
		id:           id,
		zone:         options.zone,
		shutdownCh:   make(chan struct{}),
		leakyBuckets: ratelimiter.NewLeakyBuckets(1000*60, time.Minute),
		certFile:     options.certFile,
		keyFile:      options.keyFile,
		pubPeers:     make([]string, 0, 20),
		producers:    make(map[string]Producer, 100),
		consumers:    make(map[string]Consumer, 100),
	}

	registry.Default = zk.New(options.zone, id, this.InstanceInfo())

	metaConf := zkmeta.DefaultConfig(options.zone)
	metaConf.Refresh = metaRefreshInterval
	meta.Default = zkmeta.New(metaConf)
	this.guard = newGuard(this)
	this.timer = timewheel.NewTimeWheel(time.Second, 120)

	this.manServer = newManServer(options.manHttpAddr, options.manHttpsAddr,
		options.maxClients, this)
	this.svrMetrics = NewServerMetrics(options.reporterInterval)

	if options.pubHttpAddr != "" || options.pubHttpsAddr != "" {
		this.pubServer = newPubServer(options.pubHttpAddr, options.pubHttpsAddr,
			options.maxClients, this)
		this.pubMetrics = NewPubMetrics()

		switch options.store {
		case "kafka":
			store.DefaultPubStore = kafka.NewPubStore(
				options.pubPoolCapcity, options.maxPubRetries, options.pubPoolIdleTimeout,
				&this.wg, options.debug, options.dryRun)

		case "dumb":
			store.DefaultPubStore = dumb.NewPubStore(&this.wg, options.debug)
		}
	}

	if options.subHttpAddr != "" || options.subHttpsAddr != "" {
		this.subServer = newSubServer(options.subHttpAddr, options.subHttpsAddr,
			options.maxClients, this)
		this.subMetrics = NewSubMetrics()

		switch options.store {
		case "kafka":
			store.DefaultSubStore = kafka.NewSubStore(&this.wg,
				this.subServer.closedConnCh, options.debug)

		case "dumb":
			store.DefaultSubStore = dumb.NewSubStore(&this.wg,
				this.subServer.closedConnCh, options.debug)

		}
	}

	return this
}

func (this *Gateway) InstanceInfo() []byte {
	var s map[string]string = make(map[string]string)
	s["host"] = ctx.Hostname()
	s["id"] = this.id
	s["ver"] = gafka.Version
	s["build"] = gafka.BuildId
	s["zone"] = options.zone
	s["man"] = options.manHttpAddr
	s["sman"] = options.manHttpsAddr
	s["pub"] = options.pubHttpAddr
	s["spub"] = options.pubHttpsAddr
	s["sub"] = options.subHttpAddr
	s["ssub"] = options.subHttpsAddr
	s["debug"] = options.debugHttpAddr
	d, _ := json.Marshal(s)
	return d
}

func (this *Gateway) Start() (err error) {
	signal.RegisterSignalsHandler(func(sig os.Signal) {
		this.Stop()
	}, syscall.SIGINT, syscall.SIGUSR2)

	this.startedAt = time.Now()

	startRuntimeMetrics(options.reporterInterval)

	meta.Default.Start()
	log.Trace("meta store started")

	this.guard.Start()
	log.Trace("guard started")

	this.buildRouting()
	this.manServer.Start()

	if options.debugHttpAddr != "" {
		log.Info("debug http server ready on %s", options.debugHttpAddr)

		go http.ListenAndServe(options.debugHttpAddr, nil)
	}

	if this.pubServer != nil {
		if err := store.DefaultPubStore.Start(); err != nil {
			panic(err)
		}
		log.Trace("pub store[%s] started", store.DefaultPubStore.Name())

		this.pubServer.Start()
	}
	if this.subServer != nil {
		if err := store.DefaultSubStore.Start(); err != nil {
			panic(err)
		}
		log.Trace("sub store[%s] started", store.DefaultSubStore.Name())

		this.subServer.Start()
	}

	// the last thing is to register: notify others
	if registry.Default != nil {
		if err := registry.Default.Register(); err != nil {
			panic(err)
		}
	}

	log.Info("gateway[%s:%s] ready", ctx.Hostname(), this.id)

	return nil
}

func (this *Gateway) Stop() {
	this.shutdownOnce.Do(func() {
		log.Info("stopping kateway...")

		close(this.shutdownCh)
	})

}

func (this *Gateway) ServeForever() {
	select {
	case <-this.shutdownCh:
		// the 1st thing is to deregister
		if registry.Default != nil {
			if err := registry.Default.Deregister(); err != nil {
				log.Error("Deregister: %v", err)
			}
		}

		log.Info("Deregister done, waiting for components shutdown...")
		this.wg.Wait()

		if store.DefaultPubStore != nil {
			store.DefaultPubStore.Stop()
		}
		if store.DefaultSubStore != nil {
			store.DefaultSubStore.Stop()
		}

		log.Info("all components shutdown complete")

		meta.Default.Stop()
		this.timer.Stop()
	}

}
