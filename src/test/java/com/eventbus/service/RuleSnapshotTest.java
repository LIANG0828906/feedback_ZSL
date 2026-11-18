package com.eventbus.service;

import static org.assertj.core.api.Assertions.assertThat;

import java.time.Instant;
import java.util.List;

import org.junit.jupiter.api.Test;

import com.eventbus.domain.RuleDefinition;
import com.eventbus.repo.RuleRow;
import com.eventbus.repo.SwitchRow;

class RuleSnapshotTest {

    @Test
    void choosesRuleVersionAtPerPartitionBoundary() {
        Instant now = Instant.now();
        RuleSnapshot snapshot = new RuleSnapshot(
                3,
                List.of(
                        new RuleRow(1, new RuleDefinition(List.of("ORDER_PAID"), List.of()), now),
                        new RuleRow(2, new RuleDefinition(List.of("ORDER_SHIPPED"), List.of()), now),
                        new RuleRow(3, new RuleDefinition(List.of("REFUND_REQUESTED"), List.of()), now)),
                List.of(
                        new SwitchRow(0, 0, 1, 0, now),
                        new SwitchRow(0, 1, 2, 100, now),
                        new SwitchRow(0, 2, 3, 250, now)));

        assertThat(snapshot.versionAt(0, 99)).isEqualTo(1);
        assertThat(snapshot.versionAt(0, 100)).isEqualTo(2);
        assertThat(snapshot.versionAt(0, 249)).isEqualTo(2);
        assertThat(snapshot.versionAt(0, 250)).isEqualTo(3);
        assertThat(snapshot.versionAt(1, 0)).isEqualTo(3);
    }
}
