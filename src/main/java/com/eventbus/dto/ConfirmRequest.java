package com.eventbus.dto;

public record ConfirmRequest(Integer partition, Long offset) {
}
