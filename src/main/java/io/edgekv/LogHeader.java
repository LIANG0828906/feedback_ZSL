package io.edgekv;

public record LogHeader(String chainId, long generation, int headerLength) {
}
