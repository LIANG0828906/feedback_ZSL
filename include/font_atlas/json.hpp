#pragma once

#include "font_atlas/types.hpp"

#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace font_atlas {

class Json {
public:
    enum class Type {
        Null,
        Bool,
        Number,
        String,
        Array,
        Object
    };

    using Array = std::vector<Json>;
    using Object = std::map<std::string, Json>;

    Json();
    explicit Json(bool value);
    explicit Json(double value);
    explicit Json(int value);
    explicit Json(std::int64_t value);
    explicit Json(std::uint64_t value);
    explicit Json(const char* value);
    explicit Json(std::string value);
    explicit Json(Array value);
    explicit Json(Object value);

    static Json array();
    static Json object();
    static Json null();

    Type type() const { return type_; }
    bool isNull() const { return type_ == Type::Null; }
    bool isBool() const { return type_ == Type::Bool; }
    bool isNumber() const { return type_ == Type::Number; }
    bool isString() const { return type_ == Type::String; }
    bool isArray() const { return type_ == Type::Array; }
    bool isObject() const { return type_ == Type::Object; }

    bool asBool() const;
    double asNumber() const;
    std::int64_t asInt64() const;
    int asInt() const;
    std::uint64_t asUInt64() const;
    const std::string& asString() const;
    const Array& asArray() const;
    Array& asArray();
    const Object& asObject() const;
    Object& asObject();

    const Json* find(const std::string& key) const;
    Json* find(const std::string& key);
    bool contains(const std::string& key) const;
    const Json& at(const std::string& key) const;
    Json& operator[](const std::string& key);
    void erase(const std::string& key);
    void pushBack(Json value);
    std::size_t size() const;

    std::string dump(int indent = -1) const;

    static Result<Json> parse(const std::string& text);
    static Result<Json> parseFile(const std::string& path);

private:
    Type type_;
    bool boolValue_ = false;
    double numberValue_ = 0.0;
    std::string stringValue_;
    Array arrayValue_;
    Object objectValue_;
};

bool jsonBool(const Json& object, const std::string& key, bool fallback, Diagnostic& error);
int jsonInt(const Json& object, const std::string& key, int fallback, Diagnostic& error);
std::uint64_t jsonUInt64(const Json& object, const std::string& key, std::uint64_t fallback,
                         Diagnostic& error);
double jsonDouble(const Json& object, const std::string& key, double fallback, Diagnostic& error);
std::string jsonString(const Json& object, const std::string& key, const std::string& fallback,
                       Diagnostic& error);

}  // namespace font_atlas

