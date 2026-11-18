package com.eventbus.config;

import java.time.Duration;
import java.util.Map;

import org.apache.kafka.clients.admin.AdminClient;
import org.apache.kafka.clients.admin.AdminClientConfig;
import org.apache.kafka.clients.consumer.ConsumerConfig;
import org.apache.kafka.clients.consumer.KafkaConsumer;
import org.apache.kafka.common.serialization.StringDeserializer;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

@Configuration
public class KafkaClientConfig {

    @Bean(destroyMethod = "close")
    public AdminClient kafkaAdminClient(EventBusProperties properties) {
        long requestTimeoutMs = properties.getKafka().getRequestTimeout().toMillis();
        return AdminClient.create(Map.of(
                AdminClientConfig.BOOTSTRAP_SERVERS_CONFIG,
                properties.getKafka().getBootstrapServers(),
                AdminClientConfig.CLIENT_ID_CONFIG,
                properties.getKafka().getClientId() + "-admin",
                AdminClientConfig.REQUEST_TIMEOUT_MS_CONFIG,
                Long.toString(requestTimeoutMs),
                AdminClientConfig.DEFAULT_API_TIMEOUT_MS_CONFIG,
                Long.toString(Math.max(requestTimeoutMs, Duration.ofSeconds(5).toMillis()))));
    }

    @Bean(destroyMethod = "close")
    public KafkaConsumer<String, String> eventKafkaConsumer(EventBusProperties properties) {
        long requestTimeoutMs = properties.getKafka().getRequestTimeout().toMillis();
        return new KafkaConsumer<>(Map.ofEntries(
                Map.entry(ConsumerConfig.BOOTSTRAP_SERVERS_CONFIG,
                        properties.getKafka().getBootstrapServers()),
                Map.entry(ConsumerConfig.CLIENT_ID_CONFIG,
                        properties.getKafka().getClientId() + "-reader"),
                Map.entry(ConsumerConfig.KEY_DESERIALIZER_CLASS_CONFIG,
                        StringDeserializer.class.getName()),
                Map.entry(ConsumerConfig.VALUE_DESERIALIZER_CLASS_CONFIG,
                        StringDeserializer.class.getName()),
                Map.entry(ConsumerConfig.ENABLE_AUTO_COMMIT_CONFIG, "false"),
                Map.entry(ConsumerConfig.AUTO_OFFSET_RESET_CONFIG, "earliest"),
                Map.entry(ConsumerConfig.MAX_POLL_RECORDS_CONFIG,
                        Integer.toString(properties.getKafka().getMaxPollRecords())),
                Map.entry(ConsumerConfig.MAX_PARTITION_FETCH_BYTES_CONFIG,
                        Integer.toString(properties.getKafka().getMaxPartitionFetchBytes())),
                Map.entry(ConsumerConfig.FETCH_MAX_BYTES_CONFIG,
                        Integer.toString(properties.getKafka().getFetchMaxBytes())),
                Map.entry(ConsumerConfig.REQUEST_TIMEOUT_MS_CONFIG,
                        Long.toString(requestTimeoutMs)),
                Map.entry(ConsumerConfig.DEFAULT_API_TIMEOUT_MS_CONFIG,
                        Long.toString(Math.max(requestTimeoutMs,
                                Duration.ofSeconds(5).toMillis())))));
    }
}
