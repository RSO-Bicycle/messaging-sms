package main

import (
	"bytes"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Shopify/sarama"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"github.com/rso-bicycle/messaging-email/schemas"
	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

type config struct {
	Kafka struct {
		BrokerAddr []string `yaml:"brokerAddr"`
		Topics     []struct {
			Name        string `yaml:"name"`
			PartitionID int32  `yaml:"partitionId"`
		} `yaml:"topics"`
	} `yaml:"kafka"`
	Email struct {
		FromAddress string `yaml:"sendAddress"`
		FromName    string `yaml:"fromName"`
	} `yaml:"email"`
	SendGrid struct {
		APIKey string `yaml:"apiKey"`
	} `yaml:"sendgrid"`
}

func main() {
	// Configure the logger
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err.Error())
	}

	logger.Info("running messaging email service")

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
	config.ClientID = "messaging-email"
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

	from := mail.NewEmail(cfg.Email.FromName, cfg.Email.FromAddress)
	client := sendgrid.NewSendClient(cfg.SendGrid.APIKey)

	for {
		select {
		case msg := <-ch:
			// Send an email
			go sendEmail(logger, client, from, msg)
		case <-tc:
			// Terminate the program
			return
		}
	}
}

func sendEmail(logger *zap.Logger, client *sendgrid.Client, from *mail.Email, msg *sarama.ConsumerMessage) error {
	m, err := schemas.DeserializeEmail(bytes.NewReader(msg.Value))
	if err != nil {
		return err
	}

	p := mail.NewPersonalization()
	p.AddTos(mail.NewEmail("", m.Email))

	switch m.Type {
	case schemas.EmailTypeACTIVATEUSER:
		v, err := schemas.DeserializeEmailActivateUser(bytes.NewReader(m.Data))
		if err != nil {
			return err
		}
		p.SetSubstitution("name", v.Name)
		p.SetSubstitution("activateUrl", v.ActivateUrl)
	default:
		return errors.New("unknown email template")
	}

	message := mail.NewV3Mail().SetTemplateID("email_" + strings.ToLower(m.Type.String())).AddPersonalizations(p)
	message.SetFrom(from)

	if response, err := client.Send(message); err != nil {
		return err
	} else {
		logger.Info(m.Type.String()+" mail to "+m.Email+" sent", zap.Any("response", response))
		return nil
	}
}
