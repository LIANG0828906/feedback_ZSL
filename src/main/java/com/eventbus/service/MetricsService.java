package com.eventbus.service;

import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import org.springframework.stereotype.Service;

import com.eventbus.config.EventBusProperties;
import com.eventbus.repo.GroupRow;
import com.eventbus.repo.MetricRow;
import com.eventbus.repo.ProgressRow;
import com.eventbus.repo.SubscriptionRepository;

@Service
public class MetricsService {

    private final SubscriptionRepository repository;
    private final KafkaService kafkaService;
    private final EventBusProperties properties;

    public MetricsService(SubscriptionRepository repository,
                          KafkaService kafkaService,
                          EventBusProperties properties) {
        this.repository = repository;
        this.kafkaService = kafkaService;
        this.properties = properties;
    }

    public String prometheusText() {
        List<GroupRow> groups = repository.findAllGroups();
        Map<Integer, Long> endOffsets;
        try {
            endOffsets = kafkaService.allEndOffsets();
        } catch (RuntimeException ex) {
            endOffsets = Map.of();
        }
        StringBuilder out = new StringBuilder(8_192);
        header(out, "eventbus_pull_total", "counter", "Pull requests by subscription group and partition");
        header(out, "eventbus_hit_total", "counter", "Events returned to consumers, including redeliveries");
        header(out, "eventbus_excluded_total", "counter", "Events excluded by the effective rule version");
        header(out, "eventbus_redelivery_total", "counter", "Unconfirmed events redelivered after timeout");
        header(out, "eventbus_confirmed_offset", "gauge", "Inclusive confirmed offset");
        header(out, "eventbus_backlog_events", "gauge", "Events after the confirmed offset through the latest event");
        header(out, "eventbus_rule_version", "gauge", "Current effective subscription rule version");

        for (GroupRow group : groups) {
            Map<Integer, MetricRow> metrics = new LinkedHashMap<>();
            for (MetricRow row : repository.findMetrics(group.group())) {
                metrics.put(row.partition(), row);
            }
            Map<Integer, ProgressRow> progress = new LinkedHashMap<>();
            for (ProgressRow row : repository.findProgress(group.group())) {
                progress.put(row.partition(), row);
            }
            for (int partition = 0; partition < properties.getPartitions(); partition++) {
                MetricRow metric = metrics.get(partition);
                ProgressRow row = progress.getOrDefault(
                        partition, new ProgressRow(partition, -1L, -1L, null, null));
                Long end = endOffsets.get(partition);
                long backlog = end == null ? -1 : Math.max(0, end - (row.confirmedOffset() + 1));
                String labels = labels(group.group(), partition);
                sample(out, "eventbus_pull_total", labels, metric == null ? 0 : metric.pullCount());
                sample(out, "eventbus_hit_total", labels, metric == null ? 0 : metric.hitCount());
                sample(out, "eventbus_excluded_total", labels, metric == null ? 0 : metric.excludedCount());
                sample(out, "eventbus_redelivery_total", labels, metric == null ? 0 : metric.redeliveryCount());
                sample(out, "eventbus_confirmed_offset", labels, row.confirmedOffset());
                sample(out, "eventbus_backlog_events", labels, backlog);
                sample(out, "eventbus_rule_version", labels, group.currentRuleVersion());
            }
        }
        return out.toString();
    }

    private static void header(StringBuilder out, String name, String type, String help) {
        out.append("# HELP ").append(name).append(' ').append(help).append('\n');
        out.append("# TYPE ").append(name).append(' ').append(type).append('\n');
    }

    private static void sample(StringBuilder out, String name, String labels, long value) {
        out.append(name).append(labels).append(' ').append(value).append('\n');
    }

    private static String labels(String group, int partition) {
        return "{subscription_group=\"" + escape(group) + "\",partition=\"" + partition + "\"}";
    }

    private static String escape(String value) {
        return value.replace("\\", "\\\\").replace("\"", "\\\"").replace("\n", "\\n");
    }
}
