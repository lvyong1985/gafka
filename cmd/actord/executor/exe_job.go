package executor

import (
	"fmt"
	"sync"
	"time"

	"github.com/funkygao/fae/servant/mysql"
	"github.com/funkygao/gafka/cmd/kateway/hh"
	"github.com/funkygao/gafka/cmd/kateway/job"
	jm "github.com/funkygao/gafka/cmd/kateway/job/mysql"
	"github.com/funkygao/gafka/cmd/kateway/manager"
	"github.com/funkygao/gafka/cmd/kateway/store"
	log "github.com/funkygao/log4go"
)

const (
	LagWarnThreshold   = 3  // in sec
	HandlerConcurrentN = 10 // FIXME breaks the delivery order guarantee
)

// JobExecutor polls a single JobQueue and handle each Job.
type JobExecutor struct {
	parentId       string // controller short id
	cluster, topic string
	mc             *mysql.MysqlCluster
	stopper        <-chan struct{}
	dueJobs        chan job.JobItem
	auditor        log.Logger

	// cached values
	appid string
	aid   int
	table string
	ident string
}

func NewJobExecutor(parentId, cluster, topic string, mc *mysql.MysqlCluster,
	stopper <-chan struct{}, auditor log.Logger) *JobExecutor {
	this := &JobExecutor{
		parentId: parentId,
		cluster:  cluster,
		topic:    topic,
		mc:       mc,
		stopper:  stopper,
		dueJobs:  make(chan job.JobItem, 200),
		auditor:  auditor,
	}

	return this
}

// poll mysql for due jobs and send to kafka.
func (this *JobExecutor) Run() {
	this.appid = manager.Default.TopicAppid(this.topic)
	if this.appid == "" {
		log.Warn("invalid topic: %s", this.topic)
		return
	}
	this.aid = jm.App_id(this.appid)
	this.table = jm.JobTable(this.topic)
	this.ident = this.topic

	log.Trace("starting %s", this.Ident())

	var (
		wg   sync.WaitGroup
		item job.JobItem
		tick = time.NewTicker(time.Second)
		sql  = fmt.Sprintf("SELECT job_id,payload,ctime,due_time FROM %s WHERE due_time<=?", this.table)
	)

	for i := 0; i < HandlerConcurrentN; i++ {
		wg.Add(1)
		go this.handleDueJobs(&wg)
	}

	for {
		select {
		case <-this.stopper:
			log.Debug("%s stopping", this.ident)
			wg.Wait()
			return

		case now := <-tick.C:
			rows, err := this.mc.Query(jm.AppPool, this.topic, this.aid, sql, now.Unix())
			if err != nil {
				log.Error("%s: %v", this.ident, err)
				continue
			}

			for rows.Next() {
				err = rows.Scan(&item.JobId, &item.Payload, &item.Ctime, &item.DueTime)
				if err == nil {
					log.Debug("%s due %s", this.ident, item)
					if lag := now.Unix() - item.DueTime; lag > LagWarnThreshold {
						log.Warn("%s lag %ds %s", this.ident, lag, item)
					}

					this.dueJobs <- item
				} else {
					log.Error("%s: %s", this.ident, err)
				}
			}

			if err = rows.Err(); err != nil {
				log.Error("%s: %s", this.ident, err)
			}

			rows.Close()
		}
	}

}

// TODO batch DELETE/INSERT for better performance.
func (this *JobExecutor) handleDueJobs(wg *sync.WaitGroup) {
	defer wg.Done()

	var (
		// zabbix maintains a in-memory delete queue
		// delete from history_uint where itemid=? and clock<min_clock
		sqlDeleteJob = fmt.Sprintf("DELETE FROM %s WHERE job_id=?", this.table)

		sqlInsertArchive = fmt.Sprintf("INSERT INTO %s(job_id,payload,ctime,due_time,etime,actor_id) VALUES(?,?,?,?,?,?)",
			jm.HistoryTable(this.topic))
		sqlReinject = fmt.Sprintf("INSERT INTO %s(job_id, payload, ctime, due_time) VALUES(?,?,?,?)", this.table)
	)
	for {
		select {
		case <-this.stopper:
			return

		case item := <-this.dueJobs:
			now := time.Now()
			affectedRows, _, err := this.mc.Exec(jm.AppPool, this.table, this.aid, sqlDeleteJob, item.JobId)
			if err != nil {
				log.Error("%s: %s", this.ident, err)
				continue
			}
			if affectedRows == 0 {
				// 2 possibilities:
				// - client Cancel job wins
				// - this handler is too slow and the job fetched twice in tick
				continue
			}

			log.Debug("%s land %s", this.ident, item)
			_, _, err = store.DefaultPubStore.SyncPub(this.cluster, this.topic, nil, item.Payload)
			if err != nil {
				err = hh.Default.Append(this.cluster, this.topic, nil, item.Payload)
			}
			if err != nil {
				// pub fails and hinted handoff also fails: reinject job back to mysql
				log.Error("%s: %s", this.ident, err)
				this.mc.Exec(jm.AppPool, this.table, this.aid, sqlReinject,
					item.JobId, item.Payload, item.Ctime, item.DueTime)
				continue
			}

			log.Debug("%s fired %s", this.ident, item)
			this.auditor.Trace(item.String())

			// mv job to archive table
			_, _, err = this.mc.Exec(jm.AppPool, this.table, this.aid, sqlInsertArchive,
				item.JobId, item.Payload, item.Ctime, item.DueTime, now.Unix(), this.parentId)
			if err != nil {
				log.Error("%s: %s", this.ident, err)
			} else {
				log.Debug("%s archived %s", this.ident, item)
			}

		}
	}
}

func (this *JobExecutor) Ident() string {
	return this.ident
}
