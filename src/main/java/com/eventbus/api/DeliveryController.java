package com.eventbus.api;

import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RestController;

import com.eventbus.dto.ConfirmRequest;
import com.eventbus.dto.ConfirmResponse;
import com.eventbus.dto.PullRequest;
import com.eventbus.dto.PullResponse;
import com.eventbus.service.DeliveryService;

@RestController
@RequestMapping("/api/subscriptions/{group}")
public class DeliveryController {

    private final DeliveryService service;

    public DeliveryController(DeliveryService service) {
        this.service = service;
    }

    @PostMapping("/pull")
    public PullResponse pull(@PathVariable String group,
                             @RequestBody(required = false) PullRequest request) {
        return service.pull(group, request);
    }

    @PostMapping("/confirmations")
    public ConfirmResponse confirm(@PathVariable String group,
                                   @RequestBody ConfirmRequest request) {
        return service.confirm(group, request);
    }

    @PostMapping("/confirm")
    public ConfirmResponse confirmAlias(@PathVariable String group,
                                        @RequestBody ConfirmRequest request) {
        return service.confirm(group, request);
    }
}
