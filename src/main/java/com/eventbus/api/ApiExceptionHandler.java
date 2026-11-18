package com.eventbus.api;

import java.time.Instant;
import java.util.Map;

import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.dao.DataAccessException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.http.converter.HttpMessageNotReadableException;
import org.springframework.web.bind.MissingServletRequestParameterException;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;
import org.springframework.web.method.annotation.MethodArgumentTypeMismatchException;

import com.eventbus.service.KafkaService;

@RestControllerAdvice
public class ApiExceptionHandler {

    private static final Logger log = LoggerFactory.getLogger(ApiExceptionHandler.class);

    @ExceptionHandler(ApiException.class)
    public ResponseEntity<ApiError> handleApi(ApiException ex) {
        return ResponseEntity.status(ex.getStatus()).body(new ApiError(
                ex.getCode(), ex.getMessage(), ex.getDetails(), Instant.now()));
    }

    @ExceptionHandler({HttpMessageNotReadableException.class,
            MethodArgumentTypeMismatchException.class,
            MissingServletRequestParameterException.class})
    public ResponseEntity<ApiError> handleBadRequest(Exception ex) {
        return ResponseEntity.badRequest().body(new ApiError(
                "INVALID_REQUEST",
                "request body or parameter is invalid",
                Map.of("cause", ex.getClass().getSimpleName()),
                Instant.now()));
    }

    @ExceptionHandler(DataAccessException.class)
    public ResponseEntity<ApiError> handleStorage(DataAccessException ex) {
        return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE).body(new ApiError(
                "STORAGE_UNAVAILABLE",
                "persistent storage is temporarily unavailable",
                Map.of("cause", ex.getClass().getSimpleName()),
                Instant.now()));
    }

    @ExceptionHandler({KafkaService.KafkaUnavailableException.class,
            org.apache.kafka.common.KafkaException.class})
    public ResponseEntity<ApiError> handleKafka(Exception ex) {
        return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE).body(new ApiError(
                "KAFKA_UNAVAILABLE",
                "Kafka is temporarily unavailable",
                Map.of("cause", ex.getClass().getSimpleName()),
                Instant.now()));
    }

    @ExceptionHandler(IllegalArgumentException.class)
    public ResponseEntity<ApiError> handleIllegalArgument(IllegalArgumentException ex) {
        return ResponseEntity.badRequest().body(new ApiError(
                "INVALID_REQUEST", ex.getMessage(), Map.of(), Instant.now()));
    }

    @ExceptionHandler(Exception.class)
    public ResponseEntity<ApiError> handleUnexpected(Exception ex) {
        log.error("unexpected request failure", ex);
        return ResponseEntity.status(HttpStatus.INTERNAL_SERVER_ERROR).body(new ApiError(
                "INTERNAL_ERROR",
                "unexpected service error",
                Map.of("cause", ex.getClass().getSimpleName()),
                Instant.now()));
    }
}
