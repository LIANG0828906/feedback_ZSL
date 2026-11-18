package com.eventbus.service;

import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

import org.apache.kafka.clients.admin.AdminClient;
import org.apache.kafka.clients.admin.ListOffsetsResult;
import org.apache.kafka.clients.admin.OffsetSpec;
import org.apache.kafka.clients.consumer.ConsumerRecord;
import org.apache.kafka.clients.consumer.ConsumerRecords;
import org.apache.kafka.clients.consumer.KafkaConsumer;
import org.apache.kafka.common.TopicPartition;
import org.springframework.stereotype.Service;

import com.eventbus.config.EventBusProperties;
import com.eventbus.domain.StoredEvent;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

@Service
public class KafkaService {

    private final AdminClient admin;
    private final KafkaConsumer<String, String> consumer;
    private final EventBusProperties properties;
    private final ObjectMapper objectMapper;

    public KafkaService(AdminClient admin,
                        KafkaConsumer<String, String> consumer,
                        EventBusProperties properties,
                        ObjectMapper objectMapper) {
        this.admin = admin;
        this.consumer = consumer;
        this.properties = properties;
        this.objectMapper = objectMapper;
    }

    public Map<Integer, Long> endOffsets(List<Integer> partitions) {
        Map<TopicPartition, OffsetSpec> request = new LinkedHashMap<>();
        for (Integer partition : partitions) {
            request.put(new TopicPartition(properties.getTopic(), partition), OffsetSpec.latest());
        }
        try {
            ListOffsetsResult result = admin.listOffsets(request);
            var values = result.all().get(requestTimeoutMillis(), TimeUnit.MILLISECONDS);
            Map<Integer, Long> offsets = new LinkedHashMap<>();
            for (Map.Entry<TopicPartition, ListOffsetsResult.ListOffsetsResultInfo> entry : values.entrySet()) {
                offsets.put(entry.getKey().partition(), entry.getValue().offset());
            }
            return offsets;
        } catch (InterruptedException ex) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("interrupted while reading Kafka end offsets", ex);
        } catch (Exception ex) {
            throw new KafkaUnavailableException("cannot read Kafka end offsets", ex);
        }
    }

    public Map<Integer, Long> allEndOffsets() {
        return endOffsets(java.util.stream.IntStream.range(0, properties.getPartitions())
                .boxed().toList());
    }

    public long endOffset(int partition) {
        return endOffsets(List.of(partition)).getOrDefault(partition, 0L);
    }

    public synchronized ScanResult scanPartition(int partition,
                                                 long startInclusive,
                                                 long endExclusive,
                                                 int maxRecords,
                                                 EventVisitor visitor) {
        if (startInclusive >= endExclusive || maxRecords <= 0) {
            return new ScanResult(0, startInclusive, startInclusive < endExclusive);
        }

        TopicPartition topicPartition = new TopicPartition(properties.getTopic(), partition);
        consumer.assign(List.of(topicPartition));
        consumer.seek(topicPartition, startInclusive);

        long nextOffset = startInclusive;
        int scanned = 0;
        int idlePolls = 0;
        while (nextOffset < endExclusive && scanned < maxRecords) {
            ConsumerRecords<String, String> records = consumer.poll(pollTimeout());
            boolean received = false;
            for (ConsumerRecord<String, String> record : records.records(topicPartition)) {
                if (record.offset() < nextOffset) {
                    continue;
                }
                if (record.offset() >= endExclusive) {
                    break;
                }
                received = true;
                nextOffset = record.offset() + 1;
                scanned++;
                StoredEvent event = parse(record);
                if (!visitor.onEvent(event)) {
                    return new ScanResult(scanned, nextOffset, nextOffset < endExclusive);
                }
                if (scanned >= maxRecords) {
                    break;
                }
            }
            if (!received) {
                idlePolls++;
                long position = consumer.position(topicPartition);
                if (position >= endExclusive || idlePolls >= 4) {
                    break;
                }
            } else {
                idlePolls = 0;
            }
        }
        return new ScanResult(scanned, nextOffset, nextOffset < endExclusive);
    }

    public synchronized StoredEvent readOne(int partition, long offset) {
        final StoredEvent[] result = new StoredEvent[1];
        scanPartition(partition, offset, offset + 1, 1, event -> {
            result[0] = event;
            return false;
        });
        return result[0];
    }

    public StoredEvent parse(ConsumerRecord<String, String> record) {
        String raw = record.value();
        try {
            JsonNode json = objectMapper.readTree(raw);
            if (json == null || !json.isObject()) {
                return invalid(record, "event JSON must be an object");
            }
            return new StoredEvent(
                    record.partition(),
                    record.offset(),
                    firstText(json, "eventId", "event_id", "id"),
                    firstText(json, "eventType", "event_type", "type"),
                    firstText(json, "sourceService", "source_service", "source"),
                    firstText(json, "orderId", "order_id"),
                    json,
                    true,
                    null);
        } catch (Exception ex) {
            return invalid(record, "invalid event JSON");
        }
    }

    private StoredEvent invalid(ConsumerRecord<String, String> record, String reason) {
        return new StoredEvent(
                record.partition(), record.offset(), null, null, null, null,
                objectMapper.createObjectNode(), false, reason);
    }

    private static String text(JsonNode json, String field) {
        JsonNode value = json.get(field);
        return value == null || value.isNull() ? null : value.asText();
    }

    private static String firstText(JsonNode json, String... fields) {
        for (String field : fields) {
            String value = text(json, field);
            if (value != null) {
                return value;
            }
        }
        return null;
    }

    private Duration pollTimeout() {
        return properties.getKafka().getPollTimeout();
    }

    private long requestTimeoutMillis() {
        return Math.max(properties.getKafka().getRequestTimeout().toMillis(), 1_000L);
    }

    @FunctionalInterface
    public interface EventVisitor {
        boolean onEvent(StoredEvent event);
    }

    public record ScanResult(int scanned, long nextOffset, boolean hasMore) {
    }

    public static class KafkaUnavailableException extends RuntimeException {
        public KafkaUnavailableException(String message, Throwable cause) {
            super(message, cause);
        }
    }
}
