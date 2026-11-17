#pragma once

#include <cstddef>
#include <optional>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace mosaic {

class JsonValue {
public:
    enum class Type {
        Null,
        Bool,
        Number,
        String,
        Array,
        Object,
    };

    JsonValue() = default;

    static JsonValue makeBool(bool value);
    static JsonValue makeNumber(double value);
    static JsonValue makeString(std::string value);
    static JsonValue makeArray(std::vector<JsonValue> value);
    static JsonValue makeObject(std::vector<std::pair<std::string, JsonValue>> value);

    Type type() const { return type_; }
    bool isNull() const { return type_ == Type::Null; }
    bool isBool() const { return type_ == Type::Bool; }
    bool isNumber() const { return type_ == Type::Number; }
    bool isString() const { return type_ == Type::String; }
    bool isArray() const { return type_ == Type::Array; }
    bool isObject() const { return type_ == Type::Object; }

    bool asBool() const { return bool_; }
    double asNumber() const { return number_; }
    const std::string& asString() const { return string_; }
    const std::vector<JsonValue>& asArray() const { return array_; }
    const std::vector<std::pair<std::string, JsonValue>>& asObject() const { return object_; }

    const JsonValue* find(std::string_view key) const;
    const JsonValue& at(std::size_t index) const { return array_.at(index); }
    std::size_t size() const;

private:
    Type type_ = Type::Null;
    bool bool_ = false;
    double number_ = 0.0;
    std::string string_;
    std::vector<JsonValue> array_;
    std::vector<std::pair<std::string, JsonValue>> object_;
};

struct JsonParseError {
    std::size_t offset = 0;
    std::size_t line = 1;
    std::size_t column = 1;
    std::string message;
};

struct JsonParseResult {
    std::optional<JsonValue> value;
    std::optional<JsonParseError> error;
};

JsonParseResult parseJson(std::string_view text);

std::string escapeJsonString(std::string_view text);

}  // namespace mosaic
