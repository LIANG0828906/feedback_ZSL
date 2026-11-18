package com.eventbus;

import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.boot.context.properties.ConfigurationPropertiesScan;

@SpringBootApplication
@ConfigurationPropertiesScan
public class EventBusApplication {

    public static void main(String[] args) {
        SpringApplication.run(EventBusApplication.class, args);
    }
}
