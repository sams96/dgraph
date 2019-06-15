package kafka

import (
	"fmt"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/dgraph-io/badger"
	bpb "github.com/dgraph-io/badger/pb"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/golang/glog"
	"golang.org/x/net/context"
)

var pstore *badger.DB

const (
	kafkaTopic = "dgraph"
	kafkaGroup = "dgraph"
)

func Init(db *badger.DB) {
	pstore = db
}

// Consumer represents a Sarama consumer group consumer
type Consumer struct {
	ready chan bool
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (consumer *Consumer) Setup(sarama.ConsumerGroupSession) error {
	// Mark the consumer as ready
	close(consumer.ready)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
func (consumer *Consumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
func (consumer *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession,
	claim sarama.ConsumerGroupClaim) error {
	for message := range claim.Messages() {
		kafkaMsg := &pb.KafkaMsg{}
		if err := kafkaMsg.Unmarshal(message.Value); err != nil {
			glog.Errorf("error while unmarshaling from consumed message: %v", err)
			return err
		}
		if kafkaMsg.KvList != nil {
			consumeList(kafkaMsg.KvList)
		}
		if kafkaMsg.State != nil {
			stateCb(kafkaMsg.State)
		}

		glog.V(1).Infof("Message consumed: value = %+v, timestamp = %v, topic = %s",
			kafkaMsg, message.Timestamp, message.Topic)

		// marking of the message must be done after the message has been permanently stored
		// in badger. Otherwise marking a message prematurely may result in message loss
		// if the server crashes right after the message is marked.
		session.MarkMessage(message, "")
	}

	return nil
}

func consumeList(list *bpb.KVList) {
	loader := pstore.NewLoader(16)
	for _, kv := range list.Kv {
		if err := loader.Set(kv); err != nil {
			glog.Errorf("error while setting kv %v to loader: %v", kv, err)
		}
	}
	if err := loader.Finish(); err != nil {
		glog.Errorf("error while finishing the loader: %v", err)
	}
	glog.V(1).Infof("consumed kv list: %+v", list)
}

type StateCallback func(state *pb.MembershipState)

var stateCb StateCallback

// setupKafkaSource will create a kafka consumer and and use it to receive updates
func SetupKafkaSource(cb StateCallback) {
	stateCb = cb

	sourceBrokers := Config.SourceBrokers
	glog.Infof("source kafka brokers: %v", sourceBrokers)
	if len(sourceBrokers) > 0 {
		client, err := getKafkaConsumer(sourceBrokers)
		if err != nil {
			glog.Errorf("unable to get kafka consumer and will not receive updates: %v", err)
			return
		}

		consumer := Consumer{
			ready: make(chan bool),
		}
		go func() {
			for {
				err := client.Consume(context.Background(), []string{kafkaTopic}, &consumer)
				if err != nil {
					glog.Errorf("error while consuming from kafka: %v", err)
				}
			}
		}()

		<-consumer.ready // Await till the consumer has been set up
		glog.Info("kafka consumer up and running")
	}
}

// getKafkaConsumer tries to create a consumer by connecting to Kafka at the specified brokers.
// If an error errors while creating the consumer, this function will wait and retry up to 10 times
// before giving up and returning an error
func getKafkaConsumer(sourceBrokers string) (sarama.ConsumerGroup, error) {
	config := sarama.NewConfig()
	config.Version = sarama.V2_2_0_0

	var client sarama.ConsumerGroup
	var err error
	for i := 0; i < 10; i++ {
		client, err = sarama.NewConsumerGroup(strings.Split(sourceBrokers, ","), kafkaGroup, config)
		if err == nil {
			break
		} else {
			glog.Errorf("unable to create the kafka consumer, "+
				"will retry in 5 seconds: %v", err)
			time.Sleep(5 * time.Second)
		}
	}
	return client, err
}

var producer sarama.SyncProducer

func PublishSchema(s *pb.SchemaUpdate) {
	if producer == nil {
		return
	}

	msg := &pb.KafkaMsg{
		Schema: s,
	}
	if err := produceMsg(msg); err != nil {
		glog.Errorf("error while publishing schema update to kafka: %v", err)
		return
	}

	glog.V(1).Infof("published schema update to kafka")
}

func PublishMembershipState(state *pb.MembershipState) {
	if producer == nil {
		return
	}

	msg := &pb.KafkaMsg{
		State: state,
	}
	if err := produceMsg(msg); err != nil {
		glog.Errorf("error while publishing membership state to kafka: %v", err)
		return
	}
	glog.V(1).Infof("published membership state to kafka")
}

// setupKafkaTarget will create a kafka producer and use it to send updates to
// the kafka cluster
func SetupKafkaTarget() {
	targetBrokers := Config.TargetBrokers
	glog.Infof("target kafka brokers: %v", targetBrokers)
	if len(targetBrokers) > 0 {
		var err error
		producer, err = getKafkaProducer(targetBrokers)
		if err != nil {
			glog.Errorf("unable to create the kafka sync producer, and will not publish updates")
			return
		}

		cb := func(list *bpb.KVList) {
			kafkaMsg := &pb.KafkaMsg{
				KvList: list,
			}
			if err := produceMsg(kafkaMsg); err != nil {
				glog.Errorf("error while producing to Kafka: %v", err)
				return
			}

			glog.V(1).Infof("produced a list with %d messages to kafka", len(list.Kv))
		}

		go func() {
			// The Subscribe will go into an infinite loop,
			// hence we need to run it inside a separate go routine
			if err := pstore.Subscribe(context.Background(), cb, nil); err != nil {
				glog.Errorf("error while subscribing to the pstore: %v", err)
			}
		}()

		glog.V(1).Infof("subscribed to the pstore for updates")
	}
}

func produceMsg(msg *pb.KafkaMsg) error {
	msgBytes, err := msg.Marshal()
	if err != nil {
		return fmt.Errorf("unable to marshal the kv list: %v", err)
	}
	_, _, err = producer.SendMessage(&sarama.ProducerMessage{
		Topic: kafkaTopic,
		Value: sarama.ByteEncoder(msgBytes),
	})
	return err
}

// getKafkaProducer tries to create a producer by connecting to Kafka at the specified brokers.
// If an error errors while creating the producer, this function will wait and retry up to 10 times
// before giving up and returning an error
func getKafkaProducer(targetBrokers string) (sarama.SyncProducer, error) {
	conf := sarama.NewConfig()
	conf.Producer.Return.Successes = true
	var producer sarama.SyncProducer
	var err error
	for i := 0; i < 10; i++ {
		producer, err = sarama.NewSyncProducer(strings.Split(targetBrokers, ","), conf)
		if err == nil {
			break
		} else {
			glog.Errorf("unable to create the kafka sync producer, "+
				"will retry in 5 seconds: %v", err)
			time.Sleep(5 * time.Second)
		}
	}
	return producer, err
}
