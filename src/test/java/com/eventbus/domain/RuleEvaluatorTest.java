package com.eventbus.domain;

import static org.assertj.core.api.Assertions.assertThat;

import java.util.List;

import org.junit.jupiter.api.Test;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

class RuleEvaluatorTest {

    private final ObjectMapper mapper = new ObjectMapper();
    private final RuleEvaluator evaluator = new RuleEvaluator();

    @Test
    void matchesEventTypeAndNestedPayloadConditions() throws Exception {
        JsonNode event = mapper.readTree("""
                {
                  "eventType": "ORDER_PAID",
                  "payload": {"channel": "APP", "amount": 120.50, "region": "CN-EAST"}
                }
                """);
        RuleDefinition rule = new RuleDefinition(
                List.of("ORDER_PAID", "ORDER_SHIPPED"),
                List.of(
                        new RuleCondition("payload.channel", "eq", mapper.valueToTree("APP")),
                        new RuleCondition("payload.amount", "gte", mapper.valueToTree(100)),
                        new RuleCondition("payload.region", "in",
                                mapper.valueToTree(List.of("CN-EAST", "CN-NORTH")))));

        assertThat(evaluator.matches(rule, event)).isTrue();
    }

    @Test
    void excludesByEventTypeWithoutConsumingPayload() throws Exception {
        JsonNode event = mapper.readTree("""
                {"eventType": "REFUND_REQUESTED", "payload": {"channel": "WEB"}}
                """);
        RuleDefinition rule = new RuleDefinition(
                List.of("ORDER_PAID", "ORDER_SHIPPED"), List.of());

        assertThat(evaluator.matches(rule, event)).isFalse();
    }

    @Test
    void validatesOperatorAndRegex() throws Exception {
        RuleDefinition valid = new RuleDefinition(
                List.of("ORDER_PAID"),
                List.of(new RuleCondition("payload.region", "regex",
                        mapper.valueToTree("^CN-(EAST|WEST)$"))));
        RuleDefinition invalid = new RuleDefinition(
                List.of("ORDER_PAID"),
                List.of(new RuleCondition("payload.region", "regex",
                        mapper.valueToTree("["))));

        assertThat(evaluator.validate(valid)).isNull();
        assertThat(evaluator.validate(invalid)).contains("invalid regular expression");
    }
}
