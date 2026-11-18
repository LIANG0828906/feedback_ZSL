package com.eventbus.api;

import java.util.Arrays;
import java.util.List;

import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

import com.eventbus.dto.AttributionResponse;
import com.eventbus.dto.DecisionResponse;
import com.eventbus.dto.DeliveryAuditResponse;
import com.eventbus.dto.SubscriptionStatus;
import com.eventbus.service.AttributionService;
import com.eventbus.service.DecisionService;
import com.eventbus.service.StatusService;

@RestController
@RequestMapping("/api/subscriptions/{group}")
public class StatusController {

    private final StatusService statusService;
    private final AttributionService attributionService;
    private final DecisionService decisionService;

    public StatusController(StatusService statusService,
                            AttributionService attributionService,
                            DecisionService decisionService) {
        this.statusService = statusService;
        this.attributionService = attributionService;
        this.decisionService = decisionService;
    }

    @GetMapping("/status")
    public SubscriptionStatus status(@PathVariable String group) {
        return statusService.status(group);
    }

    @GetMapping({"/attribution", "/backlog"})
    public AttributionResponse attribution(
            @PathVariable String group,
            @RequestParam(required = false) String partitions) {
        List<Integer> parsed = partitions == null || partitions.isBlank()
                ? null
                : Arrays.stream(partitions.split(","))
                        .map(String::trim)
                        .filter(value -> !value.isEmpty())
                        .map(Integer::valueOf)
                        .toList();
        return attributionService.attribute(group, parsed);
    }

    @GetMapping("/partitions/{partition}/events/{offset}/decision")
    public DecisionResponse decision(@PathVariable String group,
                                     @PathVariable int partition,
                                     @PathVariable long offset) {
        return decisionService.decision(group, partition, offset);
    }

    @GetMapping("/deliveries")
    public DeliveryAuditResponse deliveries(@PathVariable String group,
                                            @RequestParam String eventId) {
        return statusService.deliveryAudit(group, eventId);
    }
}
