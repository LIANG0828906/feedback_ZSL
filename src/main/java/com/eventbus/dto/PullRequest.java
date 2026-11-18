package com.eventbus.dto;

import java.util.List;

public record PullRequest(List<Integer> partitions, Integer maxEventsPerPartition) {
}
