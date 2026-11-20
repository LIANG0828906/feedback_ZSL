package io.edgekv;

import java.util.Collection;
import java.util.Iterator;
import java.util.function.Function;

public final class Json {
    private Json() {
    }

    public static String quote(String value) {
        if (value == null) {
            return "null";
        }
        StringBuilder out = new StringBuilder(value.length() + 2);
        out.append('"');
        for (int i = 0; i < value.length(); i++) {
            char c = value.charAt(i);
            switch (c) {
                case '"' -> out.append("\\\"");
                case '\\' -> out.append("\\\\");
                case '\b' -> out.append("\\b");
                case '\f' -> out.append("\\f");
                case '\n' -> out.append("\\n");
                case '\r' -> out.append("\\r");
                case '\t' -> out.append("\\t");
                default -> {
                    if (c < 0x20) {
                        out.append(String.format("\\u%04x", (int) c));
                    } else {
                        out.append(c);
                    }
                }
            }
        }
        return out.append('"').toString();
    }

    public static String array(Collection<?> values, Function<Object, String> encoder) {
        StringBuilder out = new StringBuilder("[");
        Iterator<?> iterator = values.iterator();
        while (iterator.hasNext()) {
            out.append(encoder.apply(iterator.next()));
            if (iterator.hasNext()) {
                out.append(',');
            }
        }
        return out.append(']').toString();
    }

    public static String entryObject(Entry entry) {
        return "{"
                + "\"key\":" + quote(entry.key())
                + ",\"value\":" + quote(entry.value())
                + ",\"version\":" + entry.version()
                + ",\"expireAt\":" + entry.expireAt()
                + "}";
    }

    public static String queryObject(QueryResult result) {
        return "{"
                + "\"key\":" + quote(result.key())
                + ",\"status\":" + quote(result.status().name())
                + ",\"value\":" + quote(result.value())
                + ",\"version\":" + result.version()
                + ",\"expireAt\":" + result.expireAt()
                + "}";
    }
}
