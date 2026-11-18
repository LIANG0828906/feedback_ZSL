package com.eventbus.service;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Service;

import com.eventbus.api.ApiException;
import com.eventbus.config.EventBusProperties;
import com.eventbus.domain.RuleDefinition;
import com.eventbus.domain.RuleEvaluator;
import com.eventbus.dto.AttributionResponse;
import com.eventbus.repo.DeliveryRow;
import com.eventbus.repo.ProgressRow;
import com.eventbus.repo.SubscriptionRepository;

@Service
public class AttributionService {

    private final SubscriptionService subscriptions;
    private final SubscriptionRepository repository;
    private final KafkaService kafkaService;
    private final RuleEvaluator ruleEvaluator;
    private final EventBusProperties properties;

    public AttributionService(SubscriptionService subscriptions,
                              SubscriptionRepository repository,
                              KafkaService kafkaService,
                              RuleEvaluator ruleEvaluator,
                              EventBusProperties properties) {
        this.subscriptions = subscriptions;
        this.repository = repository;
        this.kafkaService = kafkaService;
        this.ruleEvaluator = ruleEvaluator;
        this.properties = properties;
    }

    public AttributionResponse attribute(String group, List<Integer> requestedPartitions) {
        GroupState state = subscriptions.state(group);
        List<Integer> partitions = validatePartitions(requestedPartitions);
        Map<Integer, Long> endOffsets = kafkaService.endOffsets(partitions);
        Map<Integer, ProgressRow> progress = new LinkedHashMap<>();
        for (ProgressRow row : state.progress()) {
            progress.put(row.partition(), row);
        }

        List<AttributionResponse.PartitionAttribution> results = new ArrayList<>();
        long totalGap = 0;
        long totalExcluded = 0;
        long totalUnconfirmed = 0;
        long totalRedelivery = 0;

        for (Integer partition : partitions) {
            ProgressRow row = progress.getOrDefault(
                    partition, new ProgressRow(partition, -1L, -1L, null, null));
            long start = Math.max(row.confirmedOffset() + 1, 0);
            long endExclusive = endOffsets.getOrDefault(partition, 0L);
            long gap = Math.max(0, endExclusive - start);
            Map<Long, DeliveryRow> deliveries = gap == 0
                    ? Map.of()
                    : repository.findDeliveries(group, partition, start, endExclusive - 1);

            long[] counts = new long[3];
            if (gap > 0) {
                int scanLimit = (int) Math.min(Integer.MAX_VALUE, gap);
                kafkaService.scanPartition(partition, start, endExclusive, scanLimit, event -> {
                    long version = state.rules().versionAt(partition, event.offset());
                    RuleDefinition rule = state.rules().definitionFor(version);
                    boolean matched = event.valid() && ruleEvaluator.matches(rule, event.json());
                    if (!matched) {
                        counts[0]++;
                    } else {
                        DeliveryRow delivery = deliveries.get(event.offset());
                        if (delivery != null && delivery.deliveryCount() > 1) {
                            counts[2]++;
                        } else {
                            counts[1]++;
                        }
                    }
                    return true;
                });
            }

            long sum = counts[0] + counts[1] + counts[2];
            results.add(new AttributionResponse.PartitionAttribution(
                    partition,
                    row.confirmedOffset(),
                    endExclusive - 1,
                    gap,
                    counts[0],
                    counts[1],
                    counts[2],
                    sum,
                    sum == gap));
            totalGap += gap;
            totalExcluded += counts[0];
            totalUnconfirmed += counts[1];
            totalRedelivery += counts[2];
        }

        long totalSum = totalExcluded + totalUnconfirmed + totalRedelivery;
        return new AttributionResponse(
                group,
                state.group().currentRuleVersion(),
                List.copyOf(results),
                new AttributionResponse.AttributionTotals(
                        totalGap, totalExcluded, totalUnconfirmed, totalRedelivery,
                        totalSum, totalSum == totalGap));
    }

    private List<Integer> validatePartitions(List<Integer> requested) {
        if (requested == null || requested.isEmpty()) {
            return java.util.stream.IntStream.range(0, properties.getPartitions())
                    .boxed().toList();
        }
        requested.forEach(this::validatePartition);
        return requested.stream().distinct().sorted(Comparator.naturalOrder()).toList();
    }

    private void validatePartition(int partition) {
        if (partition < 0 || partition >= properties.getPartitions()) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_PARTITION",
                    "partition is outside the configured topic partition range");
        }
    }
}
