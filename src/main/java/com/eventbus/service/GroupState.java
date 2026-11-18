package com.eventbus.service;

import java.util.List;

import com.eventbus.repo.GroupRow;
import com.eventbus.repo.ProgressRow;

public record GroupState(
        GroupRow group,
        RuleSnapshot rules,
        List<ProgressRow> progress) {
}
