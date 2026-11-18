#pragma once

#include <cstdint>
#include <map>
#include <stdexcept>
#include <string>
#include <vector>

namespace caisson::json {

class ParseError : public std::runtime_error {
public:
    ParseError(const std::string& message, int line, int column)
        : std::runtime_error(message), line(line), column(column) {}
    int line;
    int column;
};

class Value {
public:
    enum class Type { Null, Boolean, Number, String, Array, Object };

    Type type = Type::Null;
    bool boolean = false;
    double number = 0.0;
    std::string string;
    std::vector<Value> array;
    std::map<std::string, Value> object;

    bool isNull() const { return type == Type::Null; }
    bool isObject() const { return type == Type::Object; }
    bool isArray() const { return type == Type::Array; }
    bool isString() const { return type == Type::String; }
    bool isNumber() const { return type == Type::Number; }
    bool isBoolean() const { return type == Type::Boolean; }

    const Value* find(const std::string& key) const {
        if (!isObject()) return nullptr;
        const auto it = object.find(key);
        return it == object.end() ? nullptr : &it->second;
    }

    const Value& at(const std::string& key) const {
        const Value* value = find(key);
        if (value == nullptr) throw std::out_of_range("missing JSON key: " + key);
        return *value;
    }
};

Value parse(const std::string& text);

std::string escape(const std::string& input);

}  // namespace caisson::json

