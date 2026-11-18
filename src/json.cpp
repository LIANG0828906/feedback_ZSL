#include "json.h"

#include <charconv>
#include <cmath>
#include <cstdlib>
#include <iomanip>
#include <limits>
#include <sstream>

namespace cr::json {

Error::Error(std::size_t offset, const std::string& message)
    : std::runtime_error(message + " at byte " + std::to_string(offset)), offset_(offset) {}

Value::Value() = default;
Value::Value(bool value) : type_(Type::Bool), bool_(value) {}
Value::Value(double value) : type_(Type::Number), number_(value) {}
Value::Value(int value) : type_(Type::Number), number_(static_cast<double>(value)) {}
Value::Value(std::string value) : type_(Type::String), string_(std::move(value)) {}
Value::Value(const char* value) : type_(Type::String), string_(value == nullptr ? "" : value) {}

Value Value::array() {
    Value value;
    value.type_ = Type::Array;
    return value;
}

Value Value::object() {
    Value value;
    value.type_ = Type::Object;
    return value;
}

bool Value::asBool() const {
    if (!isBool()) throw std::logic_error("JSON value is not a bool");
    return bool_;
}

double Value::asNumber() const {
    if (!isNumber()) throw std::logic_error("JSON value is not a number");
    return number_;
}

const std::string& Value::asString() const {
    if (!isString()) throw std::logic_error("JSON value is not a string");
    return string_;
}

std::string& Value::asString() {
    if (!isString()) throw std::logic_error("JSON value is not a string");
    return string_;
}

const std::vector<Value>& Value::asArray() const {
    if (!isArray()) throw std::logic_error("JSON value is not an array");
    return array_;
}

std::vector<Value>& Value::asArray() {
    if (!isArray()) throw std::logic_error("JSON value is not an array");
    return array_;
}

const std::map<std::string, Value>& Value::asObject() const {
    if (!isObject()) throw std::logic_error("JSON value is not an object");
    return object_;
}

std::map<std::string, Value>& Value::asObject() {
    if (!isObject()) throw std::logic_error("JSON value is not an object");
    return object_;
}

bool Value::has(const std::string& key) const {
    return isObject() && object_.find(key) != object_.end();
}

const Value& Value::at(const std::string& key) const {
    if (!isObject()) throw std::logic_error("JSON value is not an object");
    const auto it = object_.find(key);
    if (it == object_.end()) throw std::out_of_range("missing JSON key: " + key);
    return it->second;
}

Value& Value::at(const std::string& key) {
    if (!isObject()) throw std::logic_error("JSON value is not an object");
    const auto it = object_.find(key);
    if (it == object_.end()) throw std::out_of_range("missing JSON key: " + key);
    return it->second;
}

const Value* Value::find(const std::string& key) const {
    if (!isObject()) return nullptr;
    const auto it = object_.find(key);
    return it == object_.end() ? nullptr : &it->second;
}

Value* Value::find(const std::string& key) {
    if (!isObject()) return nullptr;
    const auto it = object_.find(key);
    return it == object_.end() ? nullptr : &it->second;
}

void Value::push(Value value) {
    if (!isArray()) throw std::logic_error("JSON value is not an array");
    array_.push_back(std::move(value));
}

void Value::set(std::string key, Value value) {
    if (!isObject()) throw std::logic_error("JSON value is not an object");
    object_[std::move(key)] = std::move(value);
}

namespace {

void appendEscaped(std::string& out, const std::string& value) {
    out.push_back('"');
    for (const unsigned char c : value) {
        switch (c) {
            case '"': out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\b': out += "\\b"; break;
            case '\f': out += "\\f"; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (c < 0x20u) {
                    constexpr char hex[] = "0123456789abcdef";
                    out += "\\u00";
                    out.push_back(hex[(c >> 4u) & 0x0fu]);
                    out.push_back(hex[c & 0x0fu]);
                } else {
                    out.push_back(static_cast<char>(c));
                }
                break;
        }
    }
    out.push_back('"');
}

void appendIndent(std::string& out, int count) {
    out.append(static_cast<std::size_t>(count), ' ');
}

}  // namespace

std::string numberToString(double value) {
    if (!std::isfinite(value)) return "null";
    if (value == 0.0) return "0";
    char buffer[64];
    const auto result = std::to_chars(buffer, buffer + sizeof(buffer), value,
                                      std::chars_format::general, 15);
    if (result.ec == std::errc()) {
        return std::string(buffer, result.ptr);
    }
    std::ostringstream fallback;
    fallback << std::setprecision(15) << value;
    return fallback.str();
}

std::string Value::canonical() const {
    std::string out;
    writeCanonical(out);
    return out;
}

void Value::writeCanonical(std::string& out) const {
    switch (type_) {
        case Type::Null:
            out += "null";
            break;
        case Type::Bool:
            out += bool_ ? "true" : "false";
            break;
        case Type::Number:
            out += numberToString(number_);
            break;
        case Type::String:
            appendEscaped(out, string_);
            break;
        case Type::Array: {
            out.push_back('[');
            bool first = true;
            for (const Value& item : array_) {
                if (!first) out.push_back(',');
                first = false;
                item.writeCanonical(out);
            }
            out.push_back(']');
            break;
        }
        case Type::Object: {
            out.push_back('{');
            bool first = true;
            for (const auto& [key, item] : object_) {
                if (!first) out.push_back(',');
                first = false;
                appendEscaped(out, key);
                out.push_back(':');
                item.writeCanonical(out);
            }
            out.push_back('}');
            break;
        }
    }
}

std::string Value::pretty(int indent) const {
    std::string out;
    writePretty(out, indent, 0);
    out.push_back('\n');
    return out;
}

void Value::writePretty(std::string& out, int indent, int depth) const {
    switch (type_) {
        case Type::Array: {
            if (array_.empty()) {
                out += "[]";
                return;
            }
            out += "[\n";
            for (std::size_t i = 0; i < array_.size(); ++i) {
                appendIndent(out, (depth + 1) * indent);
                array_[i].writePretty(out, indent, depth + 1);
                if (i + 1 < array_.size()) out.push_back(',');
                out.push_back('\n');
            }
            appendIndent(out, depth * indent);
            out.push_back(']');
            return;
        }
        case Type::Object: {
            if (object_.empty()) {
                out += "{}";
                return;
            }
            out += "{\n";
            std::size_t i = 0;
            for (const auto& [key, item] : object_) {
                appendIndent(out, (depth + 1) * indent);
                appendEscaped(out, key);
                out += ": ";
                item.writePretty(out, indent, depth + 1);
                if (++i < object_.size()) out.push_back(',');
                out.push_back('\n');
            }
            appendIndent(out, depth * indent);
            out.push_back('}');
            return;
        }
        default:
            writeCanonical(out);
            return;
    }
}

namespace {

class Parser {
public:
    explicit Parser(const std::string& text) : text_(text) {}

    Value parseDocument() {
        skipWhitespace();
        Value value = parseValue();
        skipWhitespace();
        if (pos_ != text_.size()) fail("unexpected trailing content");
        return value;
    }

private:
    const std::string& text_;
    std::size_t pos_ = 0;

    [[noreturn]] void fail(const std::string& message) const {
        throw Error(pos_, message);
    }

    void skipWhitespace() {
        while (pos_ < text_.size()) {
            const char c = text_[pos_];
            if (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
                ++pos_;
            } else {
                break;
            }
        }
    }

    char peek() const {
        if (pos_ >= text_.size()) fail("unexpected end of input");
        return text_[pos_];
    }

    void expect(char c) {
        if (pos_ >= text_.size() || text_[pos_] != c) {
            fail(std::string("expected '") + c + "'");
        }
        ++pos_;
    }

    Value parseValue() {
        skipWhitespace();
        const char c = peek();
        if (c == '{') return parseObject();
        if (c == '[') return parseArray();
        if (c == '"') return Value(parseString());
        if (c == 't') return parseLiteral("true", Value(true));
        if (c == 'f') return parseLiteral("false", Value(false));
        if (c == 'n') return parseLiteral("null", Value());
        if (c == '-' || (c >= '0' && c <= '9')) return Value(parseNumber());
        fail("unexpected character");
    }

    Value parseLiteral(const char* literal, Value value) {
        const std::string expected(literal);
        if (text_.compare(pos_, expected.size(), expected) != 0) {
            fail("invalid literal");
        }
        pos_ += expected.size();
        return value;
    }

    Value parseObject() {
        expect('{');
        Value result = Value::object();
        skipWhitespace();
        if (pos_ < text_.size() && text_[pos_] == '}') {
            ++pos_;
            return result;
        }
        while (true) {
            skipWhitespace();
            if (peek() != '"') fail("expected object key");
            std::string key = parseString();
            skipWhitespace();
            expect(':');
            Value value = parseValue();
            if (result.has(key)) fail("duplicate object key: " + key);
            result.set(std::move(key), std::move(value));
            skipWhitespace();
            if (pos_ < text_.size() && text_[pos_] == '}') {
                ++pos_;
                return result;
            }
            expect(',');
        }
    }

    Value parseArray() {
        expect('[');
        Value result = Value::array();
        skipWhitespace();
        if (pos_ < text_.size() && text_[pos_] == ']') {
            ++pos_;
            return result;
        }
        while (true) {
            result.push(parseValue());
            skipWhitespace();
            if (pos_ < text_.size() && text_[pos_] == ']') {
                ++pos_;
                return result;
            }
            expect(',');
        }
    }

    static void appendUtf8(std::string& out, std::uint32_t codepoint) {
        if (codepoint <= 0x7fu) {
            out.push_back(static_cast<char>(codepoint));
        } else if (codepoint <= 0x7ffu) {
            out.push_back(static_cast<char>(0xc0u | (codepoint >> 6u)));
            out.push_back(static_cast<char>(0x80u | (codepoint & 0x3fu)));
        } else if (codepoint <= 0xffffu) {
            out.push_back(static_cast<char>(0xe0u | (codepoint >> 12u)));
            out.push_back(static_cast<char>(0x80u | ((codepoint >> 6u) & 0x3fu)));
            out.push_back(static_cast<char>(0x80u | (codepoint & 0x3fu)));
        } else {
            out.push_back(static_cast<char>(0xf0u | (codepoint >> 18u)));
            out.push_back(static_cast<char>(0x80u | ((codepoint >> 12u) & 0x3fu)));
            out.push_back(static_cast<char>(0x80u | ((codepoint >> 6u) & 0x3fu)));
            out.push_back(static_cast<char>(0x80u | (codepoint & 0x3fu)));
        }
    }

    std::uint32_t parseHex4() {
        if (pos_ + 4 > text_.size()) fail("incomplete unicode escape");
        std::uint32_t value = 0;
        for (int i = 0; i < 4; ++i) {
            const char c = text_[pos_++];
            value <<= 4u;
            if (c >= '0' && c <= '9') value |= static_cast<std::uint32_t>(c - '0');
            else if (c >= 'a' && c <= 'f') value |= static_cast<std::uint32_t>(c - 'a' + 10);
            else if (c >= 'A' && c <= 'F') value |= static_cast<std::uint32_t>(c - 'A' + 10);
            else fail("invalid unicode escape");
        }
        return value;
    }

    std::string parseString() {
        expect('"');
        std::string out;
        while (pos_ < text_.size()) {
            const unsigned char c = static_cast<unsigned char>(text_[pos_++]);
            if (c == '"') return out;
            if (c < 0x20u) fail("control character in string");
            if (c != '\\') {
                out.push_back(static_cast<char>(c));
                continue;
            }
            if (pos_ >= text_.size()) fail("incomplete escape");
            const char esc = text_[pos_++];
            switch (esc) {
                case '"': out.push_back('"'); break;
                case '\\': out.push_back('\\'); break;
                case '/': out.push_back('/'); break;
                case 'b': out.push_back('\b'); break;
                case 'f': out.push_back('\f'); break;
                case 'n': out.push_back('\n'); break;
                case 'r': out.push_back('\r'); break;
                case 't': out.push_back('\t'); break;
                case 'u': {
                    std::uint32_t codepoint = parseHex4();
                    if (codepoint >= 0xd800u && codepoint <= 0xdbffu) {
                        if (pos_ + 2 > text_.size() || text_[pos_] != '\\' || text_[pos_ + 1] != 'u') {
                            fail("missing low surrogate");
                        }
                        pos_ += 2;
                        const std::uint32_t low = parseHex4();
                        if (low < 0xdc00u || low > 0xdfffu) fail("invalid low surrogate");
                        codepoint = 0x10000u + ((codepoint - 0xd800u) << 10u) + (low - 0xdc00u);
                    } else if (codepoint >= 0xdc00u && codepoint <= 0xdfffu) {
                        fail("unexpected low surrogate");
                    }
                    appendUtf8(out, codepoint);
                    break;
                }
                default:
                    fail("invalid escape");
            }
        }
        fail("unterminated string");
    }

    double parseNumber() {
        const std::size_t start = pos_;
        if (peek() == '-') ++pos_;
        if (pos_ >= text_.size()) fail("incomplete number");
        if (text_[pos_] == '0') {
            ++pos_;
        } else {
            if (text_[pos_] < '1' || text_[pos_] > '9') fail("invalid number");
            while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
        }
        if (pos_ < text_.size() && text_[pos_] == '.') {
            ++pos_;
            if (pos_ >= text_.size() || text_[pos_] < '0' || text_[pos_] > '9') {
                fail("invalid fraction");
            }
            while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
        }
        if (pos_ < text_.size() && (text_[pos_] == 'e' || text_[pos_] == 'E')) {
            ++pos_;
            if (pos_ < text_.size() && (text_[pos_] == '+' || text_[pos_] == '-')) ++pos_;
            if (pos_ >= text_.size() || text_[pos_] < '0' || text_[pos_] > '9') {
                fail("invalid exponent");
            }
            while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
        }
        const std::string token = text_.substr(start, pos_ - start);
        char* end = nullptr;
        const double value = std::strtod(token.c_str(), &end);
        if (end == nullptr || *end != '\0' || !std::isfinite(value)) {
            fail("number out of range");
        }
        return value;
    }
};

}  // namespace

Value parse(const std::string& text) {
    Parser parser(text);
    return parser.parseDocument();
}

}  // namespace cr::json

