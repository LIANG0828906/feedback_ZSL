#pragma once

#include <cstdint>
#include <map>
#include <stdexcept>
#include <string>
#include <vector>

namespace cr::json {

class Error : public std::runtime_error {
public:
    Error(std::size_t offset, const std::string& message);
    std::size_t offset() const noexcept { return offset_; }

private:
    std::size_t offset_;
};

class Value {
public:
    enum class Type { Null, Bool, Number, String, Array, Object };

    Value();
    explicit Value(bool value);
    explicit Value(double value);
    explicit Value(int value);
    explicit Value(std::string value);
    explicit Value(const char* value);

    static Value array();
    static Value object();

    Type type() const noexcept { return type_; }
    bool isNull() const noexcept { return type_ == Type::Null; }
    bool isBool() const noexcept { return type_ == Type::Bool; }
    bool isNumber() const noexcept { return type_ == Type::Number; }
    bool isString() const noexcept { return type_ == Type::String; }
    bool isArray() const noexcept { return type_ == Type::Array; }
    bool isObject() const noexcept { return type_ == Type::Object; }

    bool asBool() const;
    double asNumber() const;
    const std::string& asString() const;
    std::string& asString();
    const std::vector<Value>& asArray() const;
    std::vector<Value>& asArray();
    const std::map<std::string, Value>& asObject() const;
    std::map<std::string, Value>& asObject();

    bool has(const std::string& key) const;
    const Value& at(const std::string& key) const;
    Value& at(const std::string& key);
    const Value* find(const std::string& key) const;
    Value* find(const std::string& key);

    void push(Value value);
    void set(std::string key, Value value);

    std::string canonical() const;
    std::string pretty(int indent = 2) const;

private:
    Type type_ = Type::Null;
    bool bool_ = false;
    double number_ = 0.0;
    std::string string_;
    std::vector<Value> array_;
    std::map<std::string, Value> object_;

    void writeCanonical(std::string& out) const;
    void writePretty(std::string& out, int indent, int depth) const;
};

Value parse(const std::string& text);

std::string numberToString(double value);

}  // namespace cr::json

