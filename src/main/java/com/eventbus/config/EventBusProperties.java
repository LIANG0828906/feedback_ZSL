package com.eventbus.config;

import java.time.Duration;

import org.springframework.boot.context.properties.ConfigurationProperties;

@ConfigurationProperties(prefix = "eventbus")
public class EventBusProperties {

    private String topic = "order-events";
    private int partitions = 6;
    private int maxEventsPerPartition = 100;
    private int maxScanPerPartition = 50_000;
    private Duration confirmTimeout = Duration.ofSeconds(60);
    private final Kafka kafka = new Kafka();

    public String getTopic() {
        return topic;
    }

    public void setTopic(String topic) {
        this.topic = topic;
    }

    public int getPartitions() {
        return partitions;
    }

    public void setPartitions(int partitions) {
        this.partitions = partitions;
    }

    public int getMaxEventsPerPartition() {
        return maxEventsPerPartition;
    }

    public void setMaxEventsPerPartition(int maxEventsPerPartition) {
        this.maxEventsPerPartition = maxEventsPerPartition;
    }

    public int getMaxScanPerPartition() {
        return maxScanPerPartition;
    }

    public void setMaxScanPerPartition(int maxScanPerPartition) {
        this.maxScanPerPartition = maxScanPerPartition;
    }

    public Duration getConfirmTimeout() {
        return confirmTimeout;
    }

    public void setConfirmTimeout(Duration confirmTimeout) {
        this.confirmTimeout = confirmTimeout;
    }

    public Kafka getKafka() {
        return kafka;
    }

    public static class Kafka {
        private String bootstrapServers = "localhost:9092";
        private String clientId = "subscription-delivery";
        private Duration pollTimeout = Duration.ofMillis(250);
        private Duration requestTimeout = Duration.ofSeconds(5);
        private int maxPollRecords = 500;
        private int maxPartitionFetchBytes = 1_048_576;
        private int fetchMaxBytes = 4_194_304;

        public String getBootstrapServers() {
            return bootstrapServers;
        }

        public void setBootstrapServers(String bootstrapServers) {
            this.bootstrapServers = bootstrapServers;
        }

        public String getClientId() {
            return clientId;
        }

        public void setClientId(String clientId) {
            this.clientId = clientId;
        }

        public Duration getPollTimeout() {
            return pollTimeout;
        }

        public void setPollTimeout(Duration pollTimeout) {
            this.pollTimeout = pollTimeout;
        }

        public Duration getRequestTimeout() {
            return requestTimeout;
        }

        public void setRequestTimeout(Duration requestTimeout) {
            this.requestTimeout = requestTimeout;
        }

        public int getMaxPollRecords() {
            return maxPollRecords;
        }

        public void setMaxPollRecords(int maxPollRecords) {
            this.maxPollRecords = maxPollRecords;
        }

        public int getMaxPartitionFetchBytes() {
            return maxPartitionFetchBytes;
        }

        public void setMaxPartitionFetchBytes(int maxPartitionFetchBytes) {
            this.maxPartitionFetchBytes = maxPartitionFetchBytes;
        }

        public int getFetchMaxBytes() {
            return fetchMaxBytes;
        }

        public void setFetchMaxBytes(int fetchMaxBytes) {
            this.fetchMaxBytes = fetchMaxBytes;
        }
    }
}
