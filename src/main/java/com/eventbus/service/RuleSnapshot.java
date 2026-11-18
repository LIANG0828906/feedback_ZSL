package com.eventbus.service;

import java.util.Comparator;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

import com.eventbus.domain.RuleDefinition;
import com.eventbus.repo.RuleRow;
import com.eventbus.repo.SwitchRow;

public final class RuleSnapshot {

    private final long currentVersion;
    private final Map<Long, RuleDefinition> rules;
    private final Map<Integer, List<SwitchRow>> switches;

    public RuleSnapshot(long currentVersion, List<RuleRow> ruleRows, List<SwitchRow> switchRows) {
        this.currentVersion = currentVersion;
        this.rules = new HashMap<>();
        for (RuleRow rule : ruleRows) {
            rules.put(rule.version(), rule.definition());
        }
        Map<Integer, List<SwitchRow>> byPartition = new HashMap<>();
        for (SwitchRow row : switchRows) {
            byPartition.computeIfAbsent(row.partition(), ignored -> new java.util.ArrayList<>()).add(row);
        }
        for (List<SwitchRow> rows : byPartition.values()) {
            rows.sort(Comparator.comparingLong(SwitchRow::newVersion));
        }
        this.switches = byPartition;
    }

    public long currentVersion() {
        return currentVersion;
    }

    public long versionAt(int partition, long offset) {
        List<SwitchRow> rows = switches.get(partition);
        if (rows == null || rows.isEmpty()) {
            return currentVersion;
        }
        for (int index = rows.size() - 1; index >= 0; index--) {
            SwitchRow row = rows.get(index);
            if (row.boundaryOffset() <= offset) {
                return row.newVersion();
            }
        }
        return rows.get(0).oldVersion() > 0 ? rows.get(0).oldVersion() : rows.get(0).newVersion();
    }

    public RuleDefinition definitionAt(int partition, long offset) {
        return definitionFor(versionAt(partition, offset));
    }

    public RuleDefinition definitionFor(long version) {
        RuleDefinition rule = rules.get(version);
        if (rule != null) {
            return rule;
        }
        return rules.getOrDefault(currentVersion, RuleDefinition.empty());
    }

    public List<SwitchRow> switchesFor(int partition) {
        return switches.getOrDefault(partition, List.of());
    }
}
