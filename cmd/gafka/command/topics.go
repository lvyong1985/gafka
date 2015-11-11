package command

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/Shopify/sarama"
	"github.com/funkygao/gafka/config"
	"github.com/funkygao/gafka/zk"
	"github.com/funkygao/gocli"
	"github.com/funkygao/golib/color"
)

type Topics struct {
	Ui cli.Ui
}

func (this *Topics) Run(args []string) (exitCode int) {
	var (
		zone    string
		cluster string
		topic   string
	)
	cmdFlags := flag.NewFlagSet("brokers", flag.ContinueOnError)
	cmdFlags.Usage = func() { this.Ui.Output(this.Help()) }
	cmdFlags.StringVar(&zone, "z", "", "")
	cmdFlags.StringVar(&topic, "t", "", "")
	cmdFlags.StringVar(&cluster, "c", "", "")
	if err := cmdFlags.Parse(args); err != nil {
		return 1
	}

	if zone == "" {
		this.Ui.Error("empty zone not allowed")
		this.Ui.Output(this.Help())
		return 2
	}

	ensureZoneValid(zone)

	zkzone := zk.NewZkZone(zk.DefaultConfig(zone, config.ZonePath(zone)))
	if cluster != "" {
		this.displayTopicsOfCluster(cluster, zkzone)
		return
	}

	// all clusters
	zkzone.WithinClusters(func(cluster string, path string) {
		this.displayTopicsOfCluster(cluster, zkzone)
	})

	return
}

func (this *Topics) displayTopicsOfCluster(cluster string, zkzone *zk.ZkZone) {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	this.Ui.Output(cluster)
	zkcluster := zkzone.NewCluster(cluster)

	// get all alive brokers within this cluster
	brokers := zkcluster.Brokers()
	if len(brokers) == 0 {
		this.Ui.Output(fmt.Sprintf("%4s%s", " ", color.Red("empty brokers")))
		return
	}

	sortedBrokerIds := make([]string, 0, len(brokers))
	for brokerId, _ := range brokers {
		sortedBrokerIds = append(sortedBrokerIds, brokerId)
	}
	sort.Strings(sortedBrokerIds)
	for _, brokerId := range sortedBrokerIds {
		this.Ui.Output(fmt.Sprintf("%4s%s %s", " ", color.Green(brokerId),
			brokers[brokerId]))
	}

	// find 1st broker in the cluster
	// each broker in the cluster has same metadata
	var broker0 *zk.Broker
	for _, broker := range brokers {
		broker0 = broker
		break
	}

	kfkClient, err := sarama.NewClient([]string{broker0.Addr()}, sarama.NewConfig())
	if err != nil {
		this.Ui.Output(fmt.Sprintf("%5s%s %s", " ", broker0.Addr(),
			err.Error()))
		return
	}
	defer kfkClient.Close()

	topics, err := kfkClient.Topics()
	must(err)
	if len(topics) == 0 {
		this.Ui.Output(fmt.Sprintf("%5s%s", " ", color.Magenta("no topics")))
		return
	}

	this.Ui.Output(fmt.Sprintf("%80d topics", len(topics)))
	for _, topic := range topics {
		this.Ui.Output(strings.Repeat(" ", 4) + topic)

		// get partitions and check if some dead
		alivePartitions, err := kfkClient.WritablePartitions(topic)
		must(err)
		partions, err := kfkClient.Partitions(topic)
		must(err)
		if len(alivePartitions) != len(partions) {
			this.Ui.Output(fmt.Sprintf("topic[%s] has %s partitions: %+v/%+v",
				alivePartitions, color.Red("dead"), partions))
		}

		for _, partitionID := range alivePartitions {
			leader, err := kfkClient.Leader(topic, partitionID)
			must(err)

			replicas, err := kfkClient.Replicas(topic, partitionID)
			must(err)

			// TODO isr not implemented
			this.Ui.Output(fmt.Sprintf("%8d Leader:%d Replicas:%+v",
				partitionID, leader.ID(), replicas))
		}
	}
}

func (*Topics) Synopsis() string {
	return "Print available topics from Zookeeper"
}

func (*Topics) Help() string {
	help := `
Usage: gafka topics -z zone [options]

	Print available kafka topics from Zookeeper

Options:
  
  -c cluster

  -t topic
  	Topic name, regex supported.
`
	return strings.TrimSpace(help)
}
