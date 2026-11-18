package com.eventbus.domain;

import java.util.List;

public record RuleDefinition(List<String> eventTypes, List<RuleCondition> conditions) {

    public RuleDefinition {
        eventTypes = eventTypes == null ? List.of() : List.copyOf(eventTypes);
        conditions = conditions == null ? List.of() : List.copyOf(conditions);
    }

    public static RuleDefinition empty() {
        return new RuleDefinition(List.of(), List.of());
    }
}
