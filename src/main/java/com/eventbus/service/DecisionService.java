package com.eventbus.service;

import java.util.Optional;

import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Service;

import com.eventbus.api.ApiException;
import com.eventbus.config.EventBusProperties;
import com.eventbus.domain.RuleDefinition;
import com.eventbus.domain.RuleEvaluator;
import com.eventbus.domain.StoredEvent;
import com.eventbus.dto.DecisionResponse;
import com.eventbus.repo.DecisionRow;
import com.eventbus.repo.DeliveryRow;
import com.eventbus.repo.SubscriptionRepository;

@Service
public class DecisionService {

    private final SubscriptionService subscriptions;
    private final SubscriptionRepository repository;
    private final KafkaService kafkaService;
    private final RuleEvaluator ruleEvaluator;
    private final EventBusProperties properties;

    public DecisionService(SubscriptionService subscriptions,
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

    public DecisionResponse decision(String group, int partition, long offset) {
        if (partition < 0 || partition >= properties.getPartitions()) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_PARTITION",
                    "partition is outside the configured topic partition range");
        }
        if (offset < 0) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_OFFSET",
                    "offset must be non-negative");
        }
        GroupState state = subscriptions.state(group);
        Optional<DecisionRow> audited = repository.findDecision(group, partition, offset);
        Optional<DeliveryRow> delivery = repository.findDelivery(group, partition, offset);

        if (audited.isPresent()) {
            DecisionRow row = audited.get();
            DeliveryRow delivered = delivery.orElse(null);
            return new DecisionResponse(
                    group, partition, offset, row.eventId(), row.eventType(),
                    row.ruleVersion(), row.matched(), row.reason(),
                    delivered == null ? 0 : delivered.deliveryCount(),
                    delivered != null && delivered.confirmedAt() != null);
        }

        StoredEvent event = kafkaService.readOne(partition, offset);
        if (event == null) {
            throw new ApiException(HttpStatus.NOT_FOUND, "EVENT_NOT_FOUND",
                    "event is not present in the Kafka topic",
                    java.util.Map.of("partition", partition, "offset", offset));
        }
        long version = state.rules().versionAt(partition, offset);
        RuleDefinition rule = state.rules().definitionFor(version);
        boolean matched = event.valid() && ruleEvaluator.matches(rule, event.json());
        String reason = matched ? "RULE_MATCH" : (event.valid() ? "RULE_EXCLUDED" : "INVALID_EVENT");
        DeliveryRow delivered = delivery.orElse(null);
        return new DecisionResponse(
                group, partition, offset, event.eventId(), event.eventType(),
                version, matched, reason,
                delivered == null ? 0 : delivered.deliveryCount(),
                delivered != null && delivered.confirmedAt() != null);
    }
}
