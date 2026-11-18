package com.eventbus.service;

import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Service;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;

import com.eventbus.api.ApiException;
import com.eventbus.config.EventBusProperties;
import com.eventbus.domain.RuleDefinition;
import com.eventbus.domain.RuleEvaluator;
import com.eventbus.domain.StoredEvent;
import com.eventbus.dto.ConfirmRequest;
import com.eventbus.dto.ConfirmResponse;
import com.eventbus.dto.PullRequest;
import com.eventbus.dto.PullResponse;
import com.eventbus.repo.DecisionRow;
import com.eventbus.repo.DeliveryRow;
import com.eventbus.repo.GroupRow;
import com.eventbus.repo.ProgressRow;
import com.eventbus.repo.SubscriptionRepository;
import com.fasterxml.jackson.databind.JsonNode;

@Service
public class DeliveryService {

    private final SubscriptionService subscriptions;
    private final SubscriptionRepository repository;
    private final KafkaService kafkaService;
    private final RuleEvaluator ruleEvaluator;
    private final GroupLockManager locks;
    private final RedisMirror redisMirror;
    private final EventBusProperties properties;
    private final TransactionTemplate transactions;

    public DeliveryService(SubscriptionService subscriptions,
                           SubscriptionRepository repository,
                           KafkaService kafkaService,
                           RuleEvaluator ruleEvaluator,
                           GroupLockManager locks,
                           RedisMirror redisMirror,
                           EventBusProperties properties,
                           PlatformTransactionManager transactionManager) {
        this.subscriptions = subscriptions;
        this.repository = repository;
        this.kafkaService = kafkaService;
        this.ruleEvaluator = ruleEvaluator;
        this.locks = locks;
        this.redisMirror = redisMirror;
        this.properties = properties;
        this.transactions = new TransactionTemplate(transactionManager);
    }

    public PullResponse pull(String group, PullRequest request) {
        String validGroup = validateGroupName(group);
        List<Integer> partitions = validatePartitions(request);
        int maxEvents = validateMaxEvents(request);

        return locks.withGroupLock(validGroup, () -> {
            GroupState state = subscriptions.state(validGroup);
            Map<Integer, ProgressRow> progressByPartition = new LinkedHashMap<>();
            for (ProgressRow row : state.progress()) {
                progressByPartition.put(row.partition(), row);
            }

            Instant now = Instant.now();
            Map<Integer, Long> endOffsets = kafkaService.endOffsets(partitions);
            List<PullResponse.PartitionPull> partitionResults = new ArrayList<>();
            List<DeliveryRow> newDeliveries = new ArrayList<>();
            List<DeliveryRow> redeliveries = new ArrayList<>();
            List<DecisionRow> decisions = new ArrayList<>();
            List<MetricDelta> metricDeltas = new ArrayList<>();

            for (Integer partition : partitions) {
                ProgressRow progress = progressByPartition.getOrDefault(
                        partition, new ProgressRow(partition, -1L, -1L, null, now));
                long endExclusive = endOffsets.getOrDefault(partition, 0L);
                DeliveryRow pending = repository.findFirstUnconfirmed(validGroup, partition)
                        .orElse(null);

                long resultReadOffset = progress.readOffset();
                int scanned = 0;
                int excluded = 0;
                boolean hasMore = endExclusive > Math.max(progress.readOffset(), progress.confirmedOffset()) + 1;
                List<PullResponse.DeliveredEvent> events = new ArrayList<>();

                if (pending != null) {
                    Instant timeoutAt = pending.lastDeliveredAt().plus(properties.getConfirmTimeout());
                    if (!now.isBefore(timeoutAt)) {
                        redeliveries.add(pending);
                        events.add(toDeliveredEvent(pending, pending.deliveryCount() + 1, now));
                        hasMore = endExclusive > resultReadOffset + 1;
                    }
                } else {
                    long start = Math.max(progress.readOffset() + 1, progress.confirmedOffset() + 1);
                    start = Math.max(start, 0);
                    if (start < endExclusive) {
                        final long[] cursor = {start};
                        final int[] excludedCount = {0};
                        final int[] eventCount = {0};
                        KafkaService.ScanResult result = kafkaService.scanPartition(
                                partition, start, endExclusive,
                                properties.getMaxScanPerPartition(),
                                event -> {
                                    long version = state.rules().versionAt(partition, event.offset());
                                    RuleDefinition rule = state.rules().definitionFor(version);
                                    boolean matched = event.valid()
                                            && ruleEvaluator.matches(rule, event.json());
                                    String reason = matched ? "RULE_MATCH"
                                            : (event.valid() ? "RULE_EXCLUDED" : "INVALID_EVENT");
                                    decisions.add(new DecisionRow(
                                            partition, event.offset(), event.eventId(), event.eventType(),
                                            version, matched, reason, now));
                                    cursor[0] = event.offset();
                                    if (!matched) {
                                        excludedCount[0]++;
                                        return true;
                                    }

                                    DeliveryRow delivery = new DeliveryRow(
                                            partition, event.offset(), event.safeEventId(),
                                            event.eventType() == null ? "UNKNOWN" : event.eventType(),
                                            version, event.json(), now, now, 1, null);
                                    newDeliveries.add(delivery);
                                    events.add(toDeliveredEvent(event, version, 1, now));
                                    eventCount[0]++;
                                    return eventCount[0] < maxEvents;
                                });
                        scanned = result.scanned();
                        excluded = excludedCount[0];
                        resultReadOffset = Math.max(resultReadOffset, cursor[0]);
                        hasMore = result.hasMore()
                                || (eventCount[0] >= maxEvents && cursor[0] + 1 < endExclusive);
                    }
                }

                long hitCount = events.size();
                long redeliveryCount = pending != null && !events.isEmpty() ? 1 : 0;
                metricDeltas.add(new MetricDelta(
                        partition, 1, hitCount, excluded, redeliveryCount));
                partitionResults.add(new PullResponse.PartitionPull(
                        partition,
                        resultReadOffset,
                        progress.confirmedOffset(),
                        scanned,
                        excluded,
                        hasMore,
                        List.copyOf(events)));
            }

            transactions.executeWithoutResult(status -> {
                for (PullResponse.PartitionPull result : partitionResults) {
                    repository.updateAfterPull(validGroup, result.partition(), result.readOffset(), now);
                }
                for (DeliveryRow delivery : newDeliveries) {
                    repository.insertDelivery(validGroup, delivery);
                }
                for (DeliveryRow delivery : redeliveries) {
                    repository.markRedelivered(validGroup, delivery.partition(),
                            delivery.offset(), now);
                }
                repository.upsertDecisions(validGroup, decisions);
                for (MetricDelta delta : metricDeltas) {
                    repository.bumpMetrics(validGroup, delta.partition(), delta.pulls(),
                            delta.hits(), delta.excluded(), delta.redeliveries());
                }
            });

            for (PullResponse.PartitionPull result : partitionResults) {
                redisMirror.updateProgress(validGroup, result.partition(),
                        result.readOffset(), result.confirmedOffset());
            }

            return new PullResponse(validGroup, state.group().currentRuleVersion(), now,
                    List.copyOf(partitionResults));
        });
    }

    public ConfirmResponse confirm(String group, ConfirmRequest request) {
        String validGroup = validateGroupName(group);
        if (request == null || request.partition() == null || request.offset() == null) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_CONFIRMATION",
                    "partition and offset are required");
        }
        int partition = request.partition();
        long offset = request.offset();
        validatePartition(partition);
        if (offset < 0) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_CONFIRMATION",
                    "offset must be non-negative");
        }

        return locks.withGroupLock(validGroup, () -> {
            subscriptions.requireGroup(validGroup);
            Instant confirmedAt = Instant.now();
            long confirmedOffset = transactions.execute(status -> {
                repository.lockGroup(validGroup);
                ProgressRow progress = repository.findProgress(validGroup, partition)
                        .orElse(new ProgressRow(partition, -1L, -1L, null, Instant.now()));
                if (offset < progress.confirmedOffset()) {
                    throw offsetError(HttpStatus.CONFLICT, "OFFSET_ROLLBACK",
                            "offset is lower than the confirmed offset",
                            progress.confirmedOffset(), null);
                }
                if (offset == progress.confirmedOffset()) {
                    throw offsetError(HttpStatus.CONFLICT, "OFFSET_DUPLICATE",
                            "offset was already confirmed",
                            progress.confirmedOffset(), null);
                }

                DeliveryRow expected = repository.findFirstUnconfirmed(validGroup, partition)
                        .orElseThrow(() -> offsetError(HttpStatus.CONFLICT, "OFFSET_NOT_DELIVERED",
                                "offset was not delivered by this subscription group",
                                progress.confirmedOffset(), null));
                if (offset != expected.offset()) {
                    if (offset > expected.offset()) {
                        throw offsetError(HttpStatus.CONFLICT, "OFFSET_OUT_OF_ORDER",
                                "a previous delivered event is still unconfirmed",
                                progress.confirmedOffset(), expected.offset());
                    }
                    throw offsetError(HttpStatus.CONFLICT, "OFFSET_NOT_DELIVERED",
                            "offset was not delivered by this subscription group",
                            progress.confirmedOffset(), expected.offset());
                }
                int updated = repository.markConfirmed(validGroup, partition, offset, confirmedAt);
                if (updated != 1) {
                    throw offsetError(HttpStatus.CONFLICT, "OFFSET_DUPLICATE",
                            "offset was already confirmed",
                            progress.confirmedOffset(), null);
                }
                repository.advanceConfirmed(validGroup, partition, offset);
                return offset;
            });

            ProgressRow progress = repository.findProgress(validGroup, partition)
                    .orElse(new ProgressRow(partition, -1L, confirmedOffset, null, Instant.now()));
            redisMirror.updateProgress(validGroup, partition, progress.readOffset(), confirmedOffset);
            return new ConfirmResponse(validGroup, partition, offset, progress.readOffset(),
                    confirmedOffset, confirmedAt);
        });
    }

    private PullResponse.DeliveredEvent toDeliveredEvent(StoredEvent event, long version,
                                                         int deliveryCount, Instant deliveredAt) {
        JsonNode payload = event.json() == null ? null : event.json().get("payload");
        return new PullResponse.DeliveredEvent(
                event.offset(), event.safeEventId(), event.eventType(),
                event.sourceService(), event.orderId(), payload,
                version, deliveryCount, deliveredAt);
    }

    private PullResponse.DeliveredEvent toDeliveredEvent(DeliveryRow row, int deliveryCount,
                                                         Instant deliveredAt) {
        JsonNode json = row.json();
        JsonNode payload = json == null ? null : json.get("payload");
        return new PullResponse.DeliveredEvent(
                row.offset(),
                row.eventId(),
                row.eventType(),
                text(json, "sourceService"),
                text(json, "orderId"),
                payload,
                row.ruleVersion(),
                deliveryCount,
                deliveredAt);
    }

    private static String text(JsonNode json, String field) {
        if (json == null) {
            return null;
        }
        JsonNode value = json.get(field);
        return value == null || value.isNull() ? null : value.asText();
    }

    private List<Integer> validatePartitions(PullRequest request) {
        List<Integer> requested = request == null ? null : request.partitions();
        Set<Integer> partitions = new LinkedHashSet<>();
        if (requested == null || requested.isEmpty()) {
            for (int partition = 0; partition < properties.getPartitions(); partition++) {
                partitions.add(partition);
            }
        } else {
            for (Integer partition : requested) {
                if (partition == null) {
                    throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_PARTITION",
                            "partition must not be null");
                }
                validatePartition(partition);
                partitions.add(partition);
            }
        }
        return partitions.stream().sorted(Comparator.naturalOrder()).toList();
    }

    private int validateMaxEvents(PullRequest request) {
        Integer requested = request == null ? null : request.maxEventsPerPartition();
        int maxEvents = requested == null ? properties.getMaxEventsPerPartition() : requested;
        if (maxEvents < 1 || maxEvents > 1_000) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_FETCH_LIMIT",
                    "maxEventsPerPartition must be between 1 and 1000");
        }
        return maxEvents;
    }

    private void validatePartition(int partition) {
        if (partition < 0 || partition >= properties.getPartitions()) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_PARTITION",
                    "partition must be in [0, " + (properties.getPartitions() - 1) + "]",
                    Map.of("partition", partition));
        }
    }

    private String validateGroupName(String group) {
        if (group == null || !group.matches("[A-Za-z0-9._-]{1,128}")) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_GROUP_NAME",
                    "group must match [A-Za-z0-9._-]{1,128}");
        }
        return group;
    }

    private ApiException offsetError(HttpStatus status, String code, String message,
                                     long confirmedOffset, Long expectedOffset) {
        Map<String, Object> details = new LinkedHashMap<>();
        details.put("confirmedOffset", confirmedOffset);
        if (expectedOffset != null) {
            details.put("expectedOffset", expectedOffset);
        }
        details.put("retryable", true);
        return new ApiException(status, code, message, details);
    }

    private record MetricDelta(int partition, long pulls, long hits,
                               long excluded, long redeliveries) {
    }
}
