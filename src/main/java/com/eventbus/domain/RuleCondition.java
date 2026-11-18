package com.eventbus.domain;

import com.fasterxml.jackson.databind.JsonNode;

public record RuleCondition(String field, String operator, JsonNode value) {
}
