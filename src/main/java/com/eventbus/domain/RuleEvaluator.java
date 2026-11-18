package com.eventbus.domain;

import java.math.BigDecimal;
import java.util.Iterator;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.regex.Pattern;
import java.util.regex.PatternSyntaxException;

import org.springframework.stereotype.Component;

import com.fasterxml.jackson.databind.JsonNode;

@Component
public class RuleEvaluator {

    private static final Set<String> OPERATORS = Set.of(
            "eq", "ne", "gt", "gte", "lt", "lte", "in", "contains",
            "exists", "prefix", "suffix", "regex");

    public boolean matches(RuleDefinition rule, JsonNode event) {
        if (rule == null || event == null || !event.isObject()) {
            return false;
        }
        String eventType = firstText(event, "eventType", "event_type", "type");
        if (eventType == null || rule.eventTypes().stream().noneMatch(type -> type.equals(eventType))) {
            return false;
        }
        for (RuleCondition condition : rule.conditions()) {
            if (!matchesCondition(condition, event)) {
                return false;
            }
        }
        return true;
    }

    public String validate(RuleDefinition rule) {
        if (rule == null) {
            return "rule is required";
        }
        if (rule.eventTypes().isEmpty()) {
            return "rule.eventTypes must contain at least one event type";
        }
        for (String eventType : rule.eventTypes()) {
            if (eventType == null || eventType.isBlank() || eventType.length() > 128) {
                return "event type must be non-blank and at most 128 characters";
            }
        }
        for (RuleCondition condition : rule.conditions()) {
            if (condition == null) {
                return "condition must not be null";
            }
            if (condition.field() == null || condition.field().isBlank()) {
                return "condition.field must not be blank";
            }
            String operator = normalize(condition.operator());
            if (!OPERATORS.contains(operator)) {
                return "unsupported condition operator: " + condition.operator();
            }
            if (!"exists".equals(operator) && condition.value() == null) {
                return "condition.value is required for operator " + operator;
            }
            if ("in".equals(operator) && (condition.value() == null || !condition.value().isArray())) {
                return "condition.value must be an array for operator in";
            }
            if ("regex".equals(operator)) {
                if (condition.value() == null || !condition.value().isTextual()) {
                    return "condition.value must be text for operator regex";
                }
                try {
                    Pattern.compile(condition.value().asText());
                } catch (PatternSyntaxException ex) {
                    return "invalid regular expression: " + ex.getDescription();
                }
            }
        }
        return null;
    }

    private boolean matchesCondition(RuleCondition condition, JsonNode event) {
        JsonNode actual = readPath(event, condition.field());
        String operator = normalize(condition.operator());
        JsonNode expected = condition.value();

        return switch (operator) {
            case "exists" -> {
                boolean exists = actual != null && !actual.isNull();
                boolean shouldExist = expected == null || expected.asBoolean(true);
                yield shouldExist ? exists : !exists;
            }
            case "eq" -> actual != null && nodeEquals(actual, expected);
            case "ne" -> actual != null && !nodeEquals(actual, expected);
            case "gt" -> actual != null && expected != null && compare(actual, expected) > 0;
            case "gte" -> actual != null && expected != null && compare(actual, expected) >= 0;
            case "lt" -> actual != null && expected != null && compare(actual, expected) < 0;
            case "lte" -> actual != null && expected != null && compare(actual, expected) <= 0;
            case "in" -> actual != null && containsNode(expected, actual);
            case "contains" -> containsValue(actual, expected);
            case "prefix" -> actual != null && expected != null
                    && actual.asText("").startsWith(expected.asText(""));
            case "suffix" -> actual != null && expected != null
                    && actual.asText("").endsWith(expected.asText(""));
            case "regex" -> actual != null && expected != null
                    && Pattern.compile(expected.asText()).matcher(actual.asText("")).find();
            default -> false;
        };
    }

    private static String normalize(String operator) {
        String normalized = operator == null ? "" : operator.trim().toLowerCase(Locale.ROOT);
        return switch (normalized) {
            case "equals", "equal" -> "eq";
            case "not_equals", "not_equal", "neq" -> "ne";
            case "greater_than" -> "gt";
            case "greater_or_equal", "greater_than_or_equal" -> "gte";
            case "less_than" -> "lt";
            case "less_or_equal", "less_than_or_equal" -> "lte";
            case "starts_with" -> "prefix";
            case "ends_with" -> "suffix";
            default -> normalized;
        };
    }

    private static String text(JsonNode event, String field) {
        JsonNode node = event.get(field);
        return node == null || node.isNull() ? null : node.asText();
    }

    private static String firstText(JsonNode event, String... fields) {
        for (String field : fields) {
            String value = text(event, field);
            if (value != null) {
                return value;
            }
        }
        return null;
    }

    private static JsonNode readPath(JsonNode root, String path) {
        JsonNode current = root;
        String[] parts = path.split("\\.");
        int start = 0;
        if (parts.length > 1 && "payload".equals(parts[0])
                && (root.get("payload") == null || root.get("payload").isNull())) {
            start = 1;
        }
        for (int index = start; index < parts.length; index++) {
            String part = parts[index];
            if (current == null || !current.isObject()) {
                return null;
            }
            current = current.get(part);
        }
        return current;
    }

    private static boolean nodeEquals(JsonNode left, JsonNode right) {
        if (left == null || right == null) {
            return left == right;
        }
        if (left.isNumber() && right.isNumber()) {
            return left.decimalValue().compareTo(right.decimalValue()) == 0;
        }
        return left.equals(right) || left.asText().equals(right.asText());
    }

    private static int compare(JsonNode actual, JsonNode expected) {
        if (actual == null || expected == null) {
            return Integer.MIN_VALUE;
        }
        if (actual.isNumber() && expected.isNumber()) {
            return actual.decimalValue().compareTo(expected.decimalValue());
        }
        try {
            return new BigDecimal(actual.asText()).compareTo(new BigDecimal(expected.asText()));
        } catch (NumberFormatException ignored) {
            return actual.asText().compareTo(expected.asText());
        }
    }

    private static boolean containsNode(JsonNode array, JsonNode value) {
        if (array == null || !array.isArray()) {
            return false;
        }
        Iterator<JsonNode> values = array.elements();
        while (values.hasNext()) {
            if (nodeEquals(values.next(), value)) {
                return true;
            }
        }
        return false;
    }

    private static boolean containsValue(JsonNode container, JsonNode value) {
        if (container == null || value == null) {
            return false;
        }
        if (container.isArray()) {
            return containsNode(container, value);
        }
        if (container.isTextual() && value.isTextual()) {
            return container.asText().contains(value.asText());
        }
        return false;
    }

    public Map<String, String> operatorDocumentation() {
        return Map.of(
                "eq", "JSON/value equality; numbers compare numerically",
                "ne", "JSON/value inequality",
                "gt/gte/lt/lte", "numeric or lexicographic comparison",
                "in", "actual value is an element of the expected array",
                "contains", "string contains or array contains",
                "exists", "field exists and is not null",
                "prefix/suffix", "string prefix/suffix",
                "regex", "Java regular expression search");
    }
}
