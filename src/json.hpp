#pragma once

#include <cstddef>
#include <map>
#include <string>
#include <vector>

namespace sr {

class Json {
public:
    enum class Type { Null, Bool, Number, String, Array, Object };

    Json();
    explicit Json(bool value);
    explicit Json(double value);
    explicit Json(const std::string& value);

    static Json array();
    static Json object();

    Type type() const { return type_; }
    bool isNull() const { return type_ == Type::Null; }
    bool isBool() const { return type_ == Type::Bool; }
    bool isNumber() const { return type_ == Type::Number; }
    bool isString() const { return type_ == Type::String; }
    bool isArray() const { return type_ == Type::Array; }
    bool isObject() const { return type_ == Type::Object; }

    bool asBool(bool fallback = false) const { return isBool() ? bool_ : fallback; }
    double asNumber(double fallback = 0.0) const { return isNumber() ? number_ : fallback; }
    const std::string& asString() const { return string_; }

    const std::vector<Json>& items() const { return array_; }
    std::vector<Json>& items() { return array_; }
    const std::map<std::string, Json>& fields() const { return object_; }
    std::map<std::string, Json>& fields() { return object_; }

    const Json* get(const std::string& key) const;
    Json* get(const std::string& key);
    Json& set(const std::string& key, Json value);
    Json& push(Json value);

    std::string dump(int indent = 2) const;

private:
    void dumpTo(std::string& out, int indent, int depth) const;

    Type type_ = Type::Null;
    bool bool_ = false;
    double number_ = 0.0;
    std::string string_;
    std::vector<Json> array_;
    std::map<std::string, Json> object_;
};

struct JsonParseError {
    size_t line = 0;
    size_t column = 0;
    std::string message;
};

bool parseJson(const std::string& text, Json& out, JsonParseError& error);

}  // namespace sr

