package main

import (
	"bytes"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Shopify/sarama"
	"github.com/mitchellh/mapstructure"
	"github.com/nexmo-community/nexmo-go"
	"github.com/pkg/errors"
	"github.com/rso-bicycle/messaging-sms/schemas"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

type phoneConfig struct {
	FromName string `yaml:"fromName"`
}

type config struct {
	Kafka struct {
		BrokerAddr []string `yaml:"brokerAddr"`
		Topics     []struct {
			Name        string `yaml:"name"`
			PartitionID int32  `yaml:"partitionId"`
		} `yaml:"topics"`
	} `yaml:"kafka"`
	Phone phoneConfig `yaml:"phone"`
	Nexmo struct {
		APIKey    string `yaml:"apiKey"`
		APISecret string `yaml:"apiSecret"`
	} `yaml:"nexmo"`
}

func main() {
	// Configure the logger
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err.Error())
	}

	logger.Info("running messaging sms service")

	// Read the config
	v := viper.New()
	v.SetConfigName("config")
	v.SetEnvPrefix("SERVICE")
	v.AddConfigPath(".")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		logger.Fatal("loading config", zap.Error(err))
	}

	cfg := new(config)
	if err := v.Unmarshal(cfg, viper.DecoderConfigOption(func(m *mapstructure.DecoderConfig) {
		m.TagName = "yaml"
	})); err != nil {
		logger.Fatal("parsing config", zap.Error(err))
	}
	logger.Info("configuration loaded")

	// Connect to Kafka
	config := sarama.NewConfig()
	config.ClientID = "messaging-sms"
	config.Consumer.Return.Errors = true
	consumer, err := sarama.NewConsumer(cfg.Kafka.BrokerAddr, config)
	if err != nil {
		logger.Fatal("connecting to Kafka", zap.Error(err))
	}
	defer consumer.Close()
	logger.Info("successfully connected to Kafka")

	ch := make(chan *sarama.ConsumerMessage, len(cfg.Kafka.Topics))
	for _, topic := range cfg.Kafka.Topics {
		pc, err := consumer.ConsumePartition(topic.Name, topic.PartitionID, sarama.OffsetOldest)
		if err != nil {
			panic(err.Error())
		}

		logger.Info("subscribed to topic " + topic.Name)

		go func(pc sarama.PartitionConsumer) {
			for {
				select {
				case msg, ok := <-pc.Messages():
					if !ok {
						return
					}
					ch <- msg
				}
			}
		}(pc)
	}

	tc := make(chan os.Signal, 1)
	signal.Notify(tc, syscall.SIGTERM|syscall.SIGABRT|syscall.SIGINT)

	auth := nexmo.NewAuthSet()
	auth.SetAPISecret(cfg.Nexmo.APIKey, cfg.Nexmo.APISecret)
	client := nexmo.NewClient(http.DefaultClient, auth)

	for {
		select {
		case msg := <-ch:
			// Send an sms
			go sendSms(client, &cfg.Phone, msg)
		case <-tc:
			// Terminate the program
			return
		}
	}
}

func sendSms(client *nexmo.Client, cfg *phoneConfig, msg *sarama.ConsumerMessage) error {
	m, err := schemas.DeserializeSms(bytes.NewReader(msg.Value))
	if err != nil {
		return err
	}

	var text string

	switch m.Type {
	case schemas.SmsTypeMFA:
		v, err := schemas.DeserializeSmsMfa(bytes.NewReader(m.Data))
		if err != nil {
			return err
		}
		text = "RSO-" + v.Code + " is your RSO Bicycle activation code"
	default:
		return errors.New("unknown sms template")
	}

	_, _, err = client.SMS.SendSMS(nexmo.SendSMSRequest{
		To:   m.PhoneNumber,
		From: cfg.FromName,
		Text: text,
	})
	return err
}
