package com.eventbus.api;

import java.util.List;

import org.springframework.http.HttpStatus;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PathVariable;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.PutMapping;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.ResponseStatus;
import org.springframework.web.bind.annotation.RestController;

import com.eventbus.dto.CreateSubscriptionRequest;
import com.eventbus.dto.SubscriptionView;
import com.eventbus.dto.UpdateRuleRequest;
import com.eventbus.service.SubscriptionService;

@RestController
@RequestMapping("/api/subscriptions")
public class SubscriptionController {

    private final SubscriptionService service;

    public SubscriptionController(SubscriptionService service) {
        this.service = service;
    }

    @PostMapping
    @ResponseStatus(HttpStatus.CREATED)
    public SubscriptionView create(@RequestBody CreateSubscriptionRequest request) {
        return service.create(request);
    }

    @GetMapping
    public List<SubscriptionView> list() {
        return service.list();
    }

    @GetMapping("/{group}")
    public SubscriptionView get(@PathVariable String group) {
        return service.get(group);
    }

    @PutMapping("/{group}/rule")
    public SubscriptionView updateRule(@PathVariable String group,
                                       @RequestBody UpdateRuleRequest request) {
        return service.updateRule(group, request);
    }

    @PostMapping("/{group}/rules")
    public SubscriptionView updateRuleAlias(@PathVariable String group,
                                            @RequestBody UpdateRuleRequest request) {
        return service.updateRule(group, request);
    }
}
