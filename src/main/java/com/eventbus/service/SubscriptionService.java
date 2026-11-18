package com.eventbus.service;

import java.util.ArrayList;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;

import org.springframework.dao.DuplicateKeyException;
import org.springframework.http.HttpStatus;
import org.springframework.stereotype.Service;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.support.TransactionTemplate;

import com.eventbus.api.ApiException;
import com.eventbus.config.EventBusProperties;
import com.eventbus.domain.RuleCondition;
import com.eventbus.domain.RuleDefinition;
import com.eventbus.domain.RuleEvaluator;
import com.eventbus.dto.CreateSubscriptionRequest;
import com.eventbus.dto.SubscriptionView;
import com.eventbus.dto.UpdateRuleRequest;
import com.eventbus.repo.GroupRow;
import com.eventbus.repo.ProgressRow;
import com.eventbus.repo.RuleRow;
import com.eventbus.repo.SubscriptionRepository;
import com.eventbus.repo.SwitchRow;

@Service
public class SubscriptionService {

    private static final Pattern GROUP_NAME = Pattern.compile("[A-Za-z0-9._-]{1,128}");

    private final SubscriptionRepository repository;
    private final RuleEvaluator ruleEvaluator;
    private final KafkaService kafkaService;
    private final GroupLockManager locks;
    private final RedisMirror redisMirror;
    private final EventBusProperties properties;
    private final TransactionTemplate transactions;

    public SubscriptionService(SubscriptionRepository repository,
                               RuleEvaluator ruleEvaluator,
                               KafkaService kafkaService,
                               GroupLockManager locks,
                               RedisMirror redisMirror,
                               EventBusProperties properties,
                               PlatformTransactionManager transactionManager) {
        this.repository = repository;
        this.ruleEvaluator = ruleEvaluator;
        this.kafkaService = kafkaService;
        this.locks = locks;
        this.redisMirror = redisMirror;
        this.properties = properties;
        this.transactions = new TransactionTemplate(transactionManager);
    }

    public SubscriptionView create(CreateSubscriptionRequest request) {
        if (request == null) {
            throw invalidRule("request body is required");
        }
        String group = validateGroupName(request.effectiveGroupName());
        RuleDefinition rule = validatedRule(request.effectiveRule());

        return locks.withGroupLock(group, () -> {
            long version = repository.nextRuleVersion();
            try {
                transactions.executeWithoutResult(status -> {
                    repository.insertGroup(group, version);
                    repository.insertRule(group, version, rule);
                    for (int partition = 0; partition < properties.getPartitions(); partition++) {
                        repository.insertSwitch(group, partition, 0L, version, 0L);
                        repository.insertProgress(group, partition);
                    }
                });
            } catch (DuplicateKeyException ex) {
                throw new ApiException(HttpStatus.CONFLICT, "SUBSCRIPTION_EXISTS",
                        "subscription group already exists", Map.of("group", group));
            }

            GroupRow row = requireGroup(group);
            List<ProgressRow> progress = repository.findProgress(group);
            redisMirror.mirrorSubscription(row, rule, progress);
            return new SubscriptionView(group, row.currentRuleVersion(), rule,
                    row.createdAt(), row.updatedAt());
        });
    }

    public SubscriptionView updateRule(String group, UpdateRuleRequest request) {
        String validGroup = validateGroupName(group);
        if (request == null) {
            throw invalidRule("request body is required");
        }
        RuleDefinition rule = validatedRule(request.effectiveRule());
        long submittedVersion = repository.nextRuleVersion();

        return locks.withGroupLock(validGroup, () -> {
            requireGroup(validGroup);
            Map<Integer, Long> boundaries = kafkaService.allEndOffsets();
            try {
                transactions.executeWithoutResult(status -> {
                    GroupRow current = repository.lockGroup(validGroup)
                            .orElseThrow(() -> notFound(validGroup));
                    if (submittedVersion <= current.currentRuleVersion()) {
                        throw staleUpdate(validGroup, submittedVersion, current.currentRuleVersion());
                    }
                    repository.insertRule(validGroup, submittedVersion, rule);
                    for (int partition = 0; partition < properties.getPartitions(); partition++) {
                        long boundary = boundaries.getOrDefault(partition, 0L);
                        repository.insertSwitch(validGroup, partition,
                                current.currentRuleVersion(), submittedVersion, boundary);
                    }
                    repository.updateGroupVersion(validGroup, submittedVersion);
                });
            } catch (ApiException ex) {
                throw ex;
            }

            GroupRow row = requireGroup(validGroup);
            List<ProgressRow> progress = repository.findProgress(validGroup);
            redisMirror.mirrorSubscription(row, rule, progress);
            return new SubscriptionView(validGroup, row.currentRuleVersion(), rule,
                    row.createdAt(), row.updatedAt());
        });
    }

    public SubscriptionView get(String group) {
        String validGroup = validateGroupName(group);
        GroupRow row = requireGroup(validGroup);
        RuleDefinition rule = repository.findRule(validGroup, row.currentRuleVersion())
                .map(RuleRow::definition)
                .orElse(RuleDefinition.empty());
        return new SubscriptionView(validGroup, row.currentRuleVersion(), rule,
                row.createdAt(), row.updatedAt());
    }

    public List<SubscriptionView> list() {
        List<SubscriptionView> result = new ArrayList<>();
        for (GroupRow group : repository.findAllGroups()) {
            RuleDefinition rule = repository.findRule(group.group(), group.currentRuleVersion())
                    .map(RuleRow::definition)
                    .orElse(RuleDefinition.empty());
            result.add(new SubscriptionView(group.group(), group.currentRuleVersion(), rule,
                    group.createdAt(), group.updatedAt()));
        }
        return result;
    }

    public GroupState state(String group) {
        String validGroup = validateGroupName(group);
        GroupRow row = requireGroup(validGroup);
        return stateFor(row);
    }

    public GroupState stateFor(GroupRow row) {
        List<RuleRow> rules = repository.findRules(row.group());
        List<SwitchRow> switches = repository.findSwitches(row.group());
        return new GroupState(row, new RuleSnapshot(row.currentRuleVersion(), rules, switches),
                repository.findProgress(row.group()));
    }

    public GroupRow requireGroup(String group) {
        return repository.findGroup(group)
                .orElseThrow(() -> notFound(group));
    }

    private RuleDefinition validatedRule(RuleDefinition rule) {
        String error = ruleEvaluator.validate(rule);
        if (error != null) {
            throw invalidRule(error);
        }
        Set<String> eventTypes = new LinkedHashSet<>();
        for (String eventType : rule.eventTypes()) {
            eventTypes.add(eventType.trim());
        }
        List<RuleCondition> conditions = rule.conditions().stream()
                .map(condition -> new RuleCondition(
                        condition.field().trim(),
                        condition.operator().trim().toLowerCase(Locale.ROOT),
                        condition.value()))
                .toList();
        return new RuleDefinition(List.copyOf(eventTypes), conditions);
    }

    private String validateGroupName(String group) {
        if (group == null || !GROUP_NAME.matcher(group).matches()) {
            throw new ApiException(HttpStatus.BAD_REQUEST, "INVALID_GROUP_NAME",
                    "group must match [A-Za-z0-9._-]{1,128}");
        }
        return group;
    }

    private ApiException invalidRule(String message) {
        return new ApiException(HttpStatus.BAD_REQUEST, "RULE_INVALID", message);
    }

    private ApiException notFound(String group) {
        return new ApiException(HttpStatus.NOT_FOUND, "SUBSCRIPTION_NOT_FOUND",
                "subscription group does not exist", Map.of("group", group));
    }

    private ApiException staleUpdate(String group, long submitted, long current) {
        return new ApiException(HttpStatus.CONFLICT, "RULE_UPDATE_STALE",
                "a newer rule version is already effective",
                Map.of("group", group,
                        "submittedVersion", submitted,
                        "currentRuleVersion", current,
                        "applied", false));
    }
}
