#pragma once

#include <map>
#include <memory>
#include <string>
#include <vector>

namespace fill::json {

struct Value {
    enum class Type { Null, Bool, Number, String, Array, Object };

    Type type = Type::Null;
    bool boolean = false;
    double number = 0.0;
    std::string string;
    std::vector<Value> array;
    std::map<std::string, Value> object;

    bool isNull() const { return type == Type::Null; }
    bool isBool() const { return type == Type::Bool; }
    bool isNumber() const { return type == Type::Number; }
    bool isString() const { return type == Type::String; }
    bool isArray() const { return type == Type::Array; }
    bool isObject() const { return type == Type::Object; }

    const Value* find(const std::string& key) const;
};

struct ParseError {
    std::string message;
    std::size_t offset = 0;
    bool ok() const { return message.empty(); }
};

struct ParseResult {
    Value value;
    ParseError error;
};

ParseResult parse(const std::string& text);
std::string escape(const std::string& value);

}  // namespace fill::json
