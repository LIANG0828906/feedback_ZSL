package com.eventbus.dto;

import java.util.List;

import com.eventbus.domain.RuleCondition;
import com.eventbus.domain.RuleDefinition;

public record UpdateRuleRequest(
        RuleDefinition rule,
        List<String> eventTypes,
        List<RuleCondition> conditions) {

    public RuleDefinition effectiveRule() {
        if (rule != null) {
            return rule;
        }
        if (eventTypes == null && conditions == null) {
            return null;
        }
        return new RuleDefinition(
                eventTypes == null ? List.of() : eventTypes,
                conditions == null ? List.of() : conditions);
    }
}
