package com.eventbus.service;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import org.springframework.stereotype.Service;

import com.eventbus.api.ApiException;
import com.eventbus.config.EventBusProperties;
import com.eventbus.domain.RuleDefinition;
import com.eventbus.dto.DeliveryAuditResponse;
import com.eventbus.dto.SubscriptionStatus;
import com.eventbus.repo.DeliveryRow;
import com.eventbus.repo.ProgressRow;
import com.eventbus.repo.SubscriptionRepository;
import com.eventbus.repo.SwitchRow;

@Service
public class StatusService {

    private final SubscriptionService subscriptions;
    private final SubscriptionRepository repository;
    private final KafkaService kafkaService;
    private final EventBusProperties properties;

    public StatusService(SubscriptionService subscriptions,
                         SubscriptionRepository repository,
                         KafkaService kafkaService,
                         EventBusProperties properties) {
        this.subscriptions = subscriptions;
        this.repository = repository;
        this.kafkaService = kafkaService;
        this.properties = properties;
    }

    public SubscriptionStatus status(String group) {
        GroupState state = subscriptions.state(group);
        Map<Integer, Long> endOffsets;
        try {
            endOffsets = kafkaService.allEndOffsets();
        } catch (RuntimeException ex) {
            endOffsets = Map.of();
        }
        Map<Integer, ProgressRow> progress = new LinkedHashMap<>();
        for (ProgressRow row : state.progress()) {
            progress.put(row.partition(), row);
        }
        List<SubscriptionStatus.PartitionStatus> partitions = new ArrayList<>();
        long totalDuplicates = 0;
        for (int partition = 0; partition < properties.getPartitions(); partition++) {
            ProgressRow row = progress.getOrDefault(
                    partition, new ProgressRow(partition, -1L, -1L, null, null));
            Long end = endOffsets.get(partition);
            Long latest = end == null ? null : end - 1;
            Long backlog = end == null ? null : Math.max(0, end - (row.confirmedOffset() + 1));
            long duplicates = repository.duplicateDeliveryCount(group, partition);
            totalDuplicates += duplicates;
            List<SubscriptionStatus.RuleSwitchView> switches = state.rules().switchesFor(partition)
                    .stream()
                    .map(this::switchView)
                    .toList();
            partitions.add(new SubscriptionStatus.PartitionStatus(
                    partition, row.readOffset(), row.confirmedOffset(),
                    latest, backlog, duplicates, switches));
        }
        RuleDefinition currentRule = state.rules().definitionFor(state.group().currentRuleVersion());
        return new SubscriptionStatus(group, state.group().currentRuleVersion(), currentRule,
                state.group().updatedAt(), totalDuplicates, List.copyOf(partitions));
    }

    public DeliveryAuditResponse deliveryAudit(String group, String eventId) {
        subscriptions.requireGroup(group);
        if (eventId == null || eventId.isBlank()) {
            throw new ApiException(org.springframework.http.HttpStatus.BAD_REQUEST,
                    "INVALID_EVENT_ID", "eventId query parameter is required");
        }
        List<DeliveryRow> rows = repository.findDeliveriesByEventId(group, eventId);
        if (rows.isEmpty()) {
            throw new ApiException(org.springframework.http.HttpStatus.NOT_FOUND,
                    "DELIVERY_NOT_FOUND", "no delivery exists for the event id",
                    Map.of("group", group, "eventId", eventId));
        }
        long total = 0;
        for (DeliveryRow row : rows) {
            total += row.deliveryCount();
        }
        List<DeliveryAuditResponse.Record> records = rows.stream()
                .map(row -> new DeliveryAuditResponse.Record(
                        row.partition(), row.offset(), row.ruleVersion(),
                        row.deliveryCount(), row.confirmedAt() != null,
                        row.firstDeliveredAt(), row.lastDeliveredAt()))
                .toList();
        return new DeliveryAuditResponse(group, eventId, total, total - rows.size(), records);
    }

    private SubscriptionStatus.RuleSwitchView switchView(SwitchRow row) {
        return new SubscriptionStatus.RuleSwitchView(
                row.oldVersion(), row.newVersion(), row.boundaryOffset(), row.switchedAt());
    }
}
