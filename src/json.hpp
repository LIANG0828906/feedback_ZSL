#pragma once

#include <cstddef>
#include <map>
#include <stdexcept>
#include <string>
#include <vector>

namespace sj {

class Value {
public:
    enum class Type { Null, Bool, Number, String, Array, Object };

    Value() = default;
    explicit Value(bool value) : type_(Type::Bool), bool_(value) {}
    explicit Value(double value) : type_(Type::Number), number_(value) {}
    explicit Value(int value) : type_(Type::Number), number_(static_cast<double>(value)) {}
    explicit Value(std::string value) : type_(Type::String), string_(std::move(value)) {}
    explicit Value(const char* value) : type_(Type::String), string_(value == nullptr ? "" : value) {}

    static Value array() {
        Value value;
        value.type_ = Type::Array;
        return value;
    }
    static Value object() {
        Value value;
        value.type_ = Type::Object;
        return value;
    }

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
    const std::vector<Value>& asArray() const { return array_; }
    const std::map<std::string, Value>& asObject() const { return object_; }
    std::vector<Value>& asArray() { return array_; }
    std::map<std::string, Value>& asObject() { return object_; }

    bool contains(const std::string& key) const { return object_.find(key) != object_.end(); }
    const Value* find(const std::string& key) const {
        auto it = object_.find(key);
        return it == object_.end() ? nullptr : &it->second;
    }
    Value* find(const std::string& key) {
        auto it = object_.find(key);
        return it == object_.end() ? nullptr : &it->second;
    }
    Value& operator[](const std::string& key) {
        type_ = Type::Object;
        return object_[key];
    }
    const Value& operator[](const std::string& key) const { return object_.at(key); }
    void erase(const std::string& key) { object_.erase(key); }

    void push(Value value) {
        type_ = Type::Array;
        array_.push_back(std::move(value));
    }

    static Value parse(const std::string& text, std::string& error);
    std::string dump(int indent = -1) const;

private:
    Type type_ = Type::Null;
    bool bool_ = false;
    double number_ = 0.0;
    std::string string_;
    std::vector<Value> array_;
    std::map<std::string, Value> object_;

    void dumpTo(std::string& out, int indent, int depth) const;
    friend class Parser;
};

} // namespace sj

