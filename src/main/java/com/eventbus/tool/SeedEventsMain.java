package com.eventbus.tool;

import java.math.BigDecimal;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Properties;
import java.util.concurrent.Future;

import org.apache.kafka.clients.producer.KafkaProducer;
import org.apache.kafka.clients.producer.ProducerConfig;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.clients.producer.RecordMetadata;
import org.apache.kafka.common.serialization.StringSerializer;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;

public final class SeedEventsMain {

    private static final String[] EVENT_TYPES = {
            "ORDER_CREATED", "ORDER_CREATED", "ORDER_CREATED", "ORDER_CREATED", "ORDER_CREATED",
            "ORDER_PAID", "ORDER_PAID", "ORDER_PAID", "ORDER_PAID", "ORDER_PAID",
            "ORDER_SHIPPED", "ORDER_SHIPPED", "ORDER_SHIPPED", "ORDER_SHIPPED", "ORDER_SHIPPED",
            "REFUND_REQUESTED", "REFUND_REQUESTED", "REFUND_REQUESTED", "REFUND_REQUESTED",
            "RETURN_REQUESTED"
    };
    private static final String[] CHANNELS = {"WEB", "APP", "MINI_PROGRAM", "STORE"};
    private static final String[] REGIONS = {"CN-EAST", "CN-NORTH", "CN-SOUTH", "CN-WEST"};

    private SeedEventsMain() {
    }

    public static void main(String[] args) throws Exception {
        String bootstrap = env("KAFKA_BOOTSTRAP_SERVERS", "localhost:9092");
        String topic = env("KAFKA_TOPIC", "order-events");
        int count = integerArg(args, "--count", 10_000);
        int duplicateEvery = integerArg(args, "--duplicate-every", 33);
        int partitions = integerArg(args, "--partitions", 6);

        Properties properties = new Properties();
        properties.put(ProducerConfig.BOOTSTRAP_SERVERS_CONFIG, bootstrap);
        properties.put(ProducerConfig.KEY_SERIALIZER_CLASS_CONFIG, StringSerializer.class.getName());
        properties.put(ProducerConfig.VALUE_SERIALIZER_CLASS_CONFIG, StringSerializer.class.getName());
        properties.put(ProducerConfig.ACKS_CONFIG, "all");
        properties.put(ProducerConfig.ENABLE_IDEMPOTENCE_CONFIG, "true");
        properties.put(ProducerConfig.RETRIES_CONFIG, "5");
        properties.put(ProducerConfig.LINGER_MS_CONFIG, "10");
        properties.put(ProducerConfig.BATCH_SIZE_CONFIG, "32768");

        ObjectMapper mapper = new ObjectMapper();
        int duplicates = 0;
        long started = System.nanoTime();
        try (KafkaProducer<String, String> producer = new KafkaProducer<>(properties)) {
            List<Future<RecordMetadata>> pending = new ArrayList<>(512);
            for (int index = 0; index < count; index++) {
                int orderIndex = index / 7;
                int partition = orderIndex % partitions;
                String orderId = "ORD-%06d".formatted(orderIndex);
                String eventId = "EVT-%08d".formatted(index);
                String eventType = EVENT_TYPES[index % EVENT_TYPES.length];
                ObjectNode event = event(mapper, index, eventId, orderId, eventType);
                pending.add(enqueue(producer, topic, partition, orderId, event));

                if (index % duplicateEvery == 0) {
                    pending.add(enqueue(producer, topic, partition, orderId, event));
                    duplicates++;
                }
                if (pending.size() >= 500) {
                    await(pending);
                }
            }
            producer.flush();
            await(pending);
        }

        long elapsedMs = (System.nanoTime() - started) / 1_000_000L;
        System.out.printf(
                "seeded logicalEvents=%d duplicateEvents=%d totalRecords=%d partitions=%d topic=%s elapsedMs=%d%n",
                count, duplicates, count + duplicates, partitions, topic, elapsedMs);
    }

    private static ObjectNode event(ObjectMapper mapper, int index, String eventId,
                                    String orderId, String eventType) {
        ObjectNode event = mapper.createObjectNode();
        event.put("eventId", eventId);
        event.put("eventType", eventType);
        event.put("orderId", orderId);
        event.put("sourceService", sourceService(eventType));
        event.put("occurredAt", Instant.parse("2026-01-01T00:00:00Z").plusSeconds(index).toString());
        event.put("schemaVersion", 1);

        ObjectNode payload = event.putObject("payload");
        payload.put("channel", CHANNELS[(index / 3) % CHANNELS.length]);
        payload.put("region", REGIONS[(index / 11) % REGIONS.length]);
        payload.put("amount", new BigDecimal("19.90").add(BigDecimal.valueOf(index % 1000))
                .setScale(2));
        payload.put("vip", index % 5 == 0);
        payload.put("warehouse", "WH-" + ((index / 17) % 4 + 1));
        payload.put("attempt", 1);
        return event;
    }

    private static String sourceService(String eventType) {
        return switch (eventType) {
            case "ORDER_CREATED" -> "order-service";
            case "ORDER_PAID", "REFUND_REQUESTED" -> "payment-service";
            case "ORDER_SHIPPED" -> "fulfillment-service";
            case "RETURN_REQUESTED" -> "after-sales-service";
            default -> "order-service";
        };
    }

    private static Future<RecordMetadata> enqueue(KafkaProducer<String, String> producer,
                                                  String topic, int partition,
                                                  String key, ObjectNode event) {
        ProducerRecord<String, String> record = new ProducerRecord<>(
                topic, partition, key, event.toString());
        return producer.send(record);
    }

    private static void await(List<Future<RecordMetadata>> pending) throws Exception {
        for (Future<RecordMetadata> future : pending) {
            try {
                future.get();
            } catch (Exception ex) {
                throw new IllegalStateException("failed to publish seed event",
                        ex.getCause() == null ? ex : ex.getCause());
            }
        }
        pending.clear();
    }

    private static int integerArg(String[] args, String name, int defaultValue) {
        for (String arg : args) {
            if (arg.startsWith(name + "=")) {
                return Integer.parseInt(arg.substring(name.length() + 1));
            }
        }
        return defaultValue;
    }

    private static String env(String name, String defaultValue) {
        String value = System.getenv(name);
        return value == null || value.isBlank() ? defaultValue : value;
    }
}
