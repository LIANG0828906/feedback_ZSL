#include "json.hpp"

#include <cmath>
#include <cstdint>
#include <cstdlib>
#include <iomanip>
#include <limits>
#include <sstream>

namespace sr {

Json::Json() = default;
Json::Json(bool value) : type_(Type::Bool), bool_(value) {}
Json::Json(double value) : type_(Type::Number), number_(value) {}
Json::Json(const std::string& value) : type_(Type::String), string_(value) {}

Json Json::array() {
    Json value;
    value.type_ = Type::Array;
    return value;
}

Json Json::object() {
    Json value;
    value.type_ = Type::Object;
    return value;
}

const Json* Json::get(const std::string& key) const {
    if (!isObject()) {
        return nullptr;
    }
    const auto it = object_.find(key);
    return it == object_.end() ? nullptr : &it->second;
}

Json* Json::get(const std::string& key) {
    if (!isObject()) {
        return nullptr;
    }
    const auto it = object_.find(key);
    return it == object_.end() ? nullptr : &it->second;
}

Json& Json::set(const std::string& key, Json value) {
    if (!isObject()) {
        type_ = Type::Object;
    }
    object_[key] = std::move(value);
    return *this;
}

Json& Json::push(Json value) {
    if (!isArray()) {
        type_ = Type::Array;
    }
    array_.push_back(std::move(value));
    return *this;
}

namespace {

void appendEscaped(std::string& out, const std::string& text) {
    out.push_back('"');
    for (const unsigned char c : text) {
        switch (c) {
            case '"':
                out += "\\\"";
                break;
            case '\\':
                out += "\\\\";
                break;
            case '\b':
                out += "\\b";
                break;
            case '\f':
                out += "\\f";
                break;
            case '\n':
                out += "\\n";
                break;
            case '\r':
                out += "\\r";
                break;
            case '\t':
                out += "\\t";
                break;
            default:
                if (c < 0x20U) {
                    static const char* hex = "0123456789abcdef";
                    out += "\\u00";
                    out.push_back(hex[c >> 4U]);
                    out.push_back(hex[c & 0x0fU]);
                } else {
                    out.push_back(static_cast<char>(c));
                }
        }
    }
    out.push_back('"');
}

void indent(std::string& out, int amount) { out.append(static_cast<size_t>(amount), ' '); }

}  // namespace

std::string Json::dump(int indentSize) const {
    std::string out;
    dumpTo(out, indentSize, 0);
    out.push_back('\n');
    return out;
}

void Json::dumpTo(std::string& out, int indentSize, int depth) const {
    switch (type_) {
        case Type::Null:
            out += "null";
            return;
        case Type::Bool:
            out += bool_ ? "true" : "false";
            return;
        case Type::Number: {
            if (!std::isfinite(number_)) {
                out += "0";
                return;
            }
            if (number_ == 0.0) {
                out += "0";
                return;
            }
            const bool integral = std::floor(number_) == number_ &&
                                  std::abs(number_) <= 9007199254740992.0;
            std::ostringstream stream;
            if (integral) {
                stream << std::fixed << std::setprecision(0) << number_;
            } else {
                stream << std::setprecision(15) << number_;
            }
            out += stream.str();
            return;
        }
        case Type::String:
            appendEscaped(out, string_);
            return;
        case Type::Array: {
            if (array_.empty()) {
                out += "[]";
                return;
            }
            out += "[\n";
            for (size_t i = 0; i < array_.size(); ++i) {
                indent(out, (depth + 1) * indentSize);
                array_[i].dumpTo(out, indentSize, depth + 1);
                if (i + 1 < array_.size()) {
                    out.push_back(',');
                }
                out.push_back('\n');
            }
            indent(out, depth * indentSize);
            out.push_back(']');
            return;
        }
        case Type::Object: {
            if (object_.empty()) {
                out += "{}";
                return;
            }
            out += "{\n";
            size_t i = 0;
            for (const auto& entry : object_) {
                indent(out, (depth + 1) * indentSize);
                appendEscaped(out, entry.first);
                out += ": ";
                entry.second.dumpTo(out, indentSize, depth + 1);
                if (++i < object_.size()) {
                    out.push_back(',');
                }
                out.push_back('\n');
            }
            indent(out, depth * indentSize);
            out.push_back('}');
            return;
        }
    }
}

namespace {

class Parser {
public:
    Parser(const std::string& text, JsonParseError& error) : text_(text), error_(error) {}

    bool parse(Json& out) {
        skipWhitespace();
        if (!parseValue(out, 0)) {
            return false;
        }
        skipWhitespace();
        if (!atEnd()) {
            fail("unexpected trailing content");
            return false;
        }
        return true;
    }

private:
    bool atEnd() const { return pos_ >= text_.size(); }
    char peek() const { return atEnd() ? '\0' : text_[pos_]; }

    void advance() {
        if (atEnd()) {
            return;
        }
        if (text_[pos_] == '\n') {
            ++line_;
            column_ = 1;
        } else {
            ++column_;
        }
        ++pos_;
    }

    void skipWhitespace() {
        while (!atEnd()) {
            const char c = peek();
            if (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
                advance();
            } else {
                break;
            }
        }
    }

    bool fail(const std::string& message) {
        error_.line = line_;
        error_.column = column_;
        error_.message = message;
        return false;
    }

    bool parseValue(Json& out, int depth) {
        if (depth > 128) {
            return fail("JSON nesting exceeds 128 levels");
        }
        skipWhitespace();
        if (atEnd()) {
            return fail("unexpected end of input");
        }
        switch (peek()) {
            case '{':
                return parseObject(out, depth + 1);
            case '[':
                return parseArray(out, depth + 1);
            case '"': {
                std::string text;
                if (!parseString(text)) {
                    return false;
                }
                out = Json(text);
                return true;
            }
            case 't':
                return parseLiteral("true", Json(true), out);
            case 'f':
                return parseLiteral("false", Json(false), out);
            case 'n':
                return parseLiteral("null", Json(), out);
            default:
                if (peek() == '-' || (peek() >= '0' && peek() <= '9')) {
                    return parseNumber(out);
                }
                return fail("unexpected character in JSON value");
        }
    }

    bool parseLiteral(const char* literal, Json value, Json& out) {
        for (const char* p = literal; *p != '\0'; ++p) {
            if (atEnd() || peek() != *p) {
                return fail(std::string("invalid literal, expected ") + literal);
            }
            advance();
        }
        out = std::move(value);
        return true;
    }

    bool parseNumber(Json& out) {
        const size_t start = pos_;
        if (peek() == '-') {
            advance();
        }
        if (atEnd()) {
            return fail("invalid number");
        }
        if (peek() == '0') {
            advance();
        } else if (peek() >= '1' && peek() <= '9') {
            while (!atEnd() && peek() >= '0' && peek() <= '9') {
                advance();
            }
        } else {
            return fail("invalid number");
        }
        if (!atEnd() && peek() == '.') {
            advance();
            if (atEnd() || peek() < '0' || peek() > '9') {
                return fail("invalid fraction in number");
            }
            while (!atEnd() && peek() >= '0' && peek() <= '9') {
                advance();
            }
        }
        if (!atEnd() && (peek() == 'e' || peek() == 'E')) {
            advance();
            if (!atEnd() && (peek() == '+' || peek() == '-')) {
                advance();
            }
            if (atEnd() || peek() < '0' || peek() > '9') {
                return fail("invalid exponent in number");
            }
            while (!atEnd() && peek() >= '0' && peek() <= '9') {
                advance();
            }
        }
        const std::string token = text_.substr(start, pos_ - start);
        char* end = nullptr;
        const double value = std::strtod(token.c_str(), &end);
        if (end == nullptr || *end != '\0' || !std::isfinite(value)) {
            return fail("number is not finite or cannot be represented");
        }
        out = Json(value);
        return true;
    }

    static void appendUtf8(std::string& out, uint32_t codePoint) {
        if (codePoint <= 0x7fU) {
            out.push_back(static_cast<char>(codePoint));
        } else if (codePoint <= 0x7ffU) {
            out.push_back(static_cast<char>(0xc0U | (codePoint >> 6U)));
            out.push_back(static_cast<char>(0x80U | (codePoint & 0x3fU)));
        } else if (codePoint <= 0xffffU) {
            out.push_back(static_cast<char>(0xe0U | (codePoint >> 12U)));
            out.push_back(static_cast<char>(0x80U | ((codePoint >> 6U) & 0x3fU)));
            out.push_back(static_cast<char>(0x80U | (codePoint & 0x3fU)));
        } else {
            out.push_back(static_cast<char>(0xf0U | (codePoint >> 18U)));
            out.push_back(static_cast<char>(0x80U | ((codePoint >> 12U) & 0x3fU)));
            out.push_back(static_cast<char>(0x80U | ((codePoint >> 6U) & 0x3fU)));
            out.push_back(static_cast<char>(0x80U | (codePoint & 0x3fU)));
        }
    }

    bool parseHex4(uint32_t& value) {
        value = 0;
        for (int i = 0; i < 4; ++i) {
            if (atEnd()) {
                return fail("unterminated unicode escape");
            }
            const char c = peek();
            uint32_t digit = 0;
            if (c >= '0' && c <= '9') {
                digit = static_cast<uint32_t>(c - '0');
            } else if (c >= 'a' && c <= 'f') {
                digit = static_cast<uint32_t>(c - 'a' + 10);
            } else if (c >= 'A' && c <= 'F') {
                digit = static_cast<uint32_t>(c - 'A' + 10);
            } else {
                return fail("invalid unicode escape");
            }
            value = (value << 4U) | digit;
            advance();
        }
        return true;
    }

    bool parseString(std::string& out) {
        if (peek() != '"') {
            return fail("expected string");
        }
        advance();
        while (!atEnd()) {
            const unsigned char c = static_cast<unsigned char>(peek());
            if (c == '"') {
                advance();
                return true;
            }
            if (c < 0x20U) {
                return fail("control character in string");
            }
            if (c != '\\') {
                out.push_back(static_cast<char>(c));
                advance();
                continue;
            }
            advance();
            if (atEnd()) {
                return fail("unterminated escape sequence");
            }
            const char escaped = peek();
            advance();
            switch (escaped) {
                case '"':
                    out.push_back('"');
                    break;
                case '\\':
                    out.push_back('\\');
                    break;
                case '/':
                    out.push_back('/');
                    break;
                case 'b':
                    out.push_back('\b');
                    break;
                case 'f':
                    out.push_back('\f');
                    break;
                case 'n':
                    out.push_back('\n');
                    break;
                case 'r':
                    out.push_back('\r');
                    break;
                case 't':
                    out.push_back('\t');
                    break;
                case 'u': {
                    uint32_t high = 0;
                    if (!parseHex4(high)) {
                        return false;
                    }
                    uint32_t codePoint = high;
                    if (high >= 0xd800U && high <= 0xdbffU) {
                        if (atEnd() || peek() != '\\') {
                            return fail("missing low surrogate");
                        }
                        advance();
                        if (atEnd() || peek() != 'u') {
                            return fail("missing low surrogate");
                        }
                        advance();
                        uint32_t low = 0;
                        if (!parseHex4(low)) {
                            return false;
                        }
                        if (low < 0xdc00U || low > 0xdfffU) {
                            return fail("invalid low surrogate");
                        }
                        codePoint = 0x10000U + ((high - 0xd800U) << 10U) + (low - 0xdc00U);
                    } else if (high >= 0xdc00U && high <= 0xdfffU) {
                        return fail("unexpected low surrogate");
                    }
                    appendUtf8(out, codePoint);
                    break;
                }
                default:
                    return fail("invalid escape sequence");
            }
        }
        return fail("unterminated string");
    }

    bool parseArray(Json& out, int depth) {
        advance();
        skipWhitespace();
        out = Json::array();
        if (!atEnd() && peek() == ']') {
            advance();
            return true;
        }
        while (true) {
            Json value;
            if (!parseValue(value, depth)) {
                return false;
            }
            out.push(std::move(value));
            skipWhitespace();
            if (atEnd()) {
                return fail("unterminated array");
            }
            if (peek() == ']') {
                advance();
                return true;
            }
            if (peek() != ',') {
                return fail("expected ',' or ']' in array");
            }
            advance();
        }
    }

    bool parseObject(Json& out, int depth) {
        advance();
        skipWhitespace();
        out = Json::object();
        if (!atEnd() && peek() == '}') {
            advance();
            return true;
        }
        while (true) {
            skipWhitespace();
            if (atEnd() || peek() != '"') {
                return fail("expected string key in object");
            }
            std::string key;
            if (!parseString(key)) {
                return false;
            }
            if (out.get(key) != nullptr) {
                return fail("duplicate object key: " + key);
            }
            skipWhitespace();
            if (atEnd() || peek() != ':') {
                return fail("expected ':' after object key");
            }
            advance();
            Json value;
            if (!parseValue(value, depth)) {
                return false;
            }
            out.set(key, std::move(value));
            skipWhitespace();
            if (atEnd()) {
                return fail("unterminated object");
            }
            if (peek() == '}') {
                advance();
                return true;
            }
            if (peek() != ',') {
                return fail("expected ',' or '}' in object");
            }
            advance();
        }
    }

    const std::string& text_;
    JsonParseError& error_;
    size_t pos_ = 0;
    size_t line_ = 1;
    size_t column_ = 1;
};

}  // namespace

bool parseJson(const std::string& text, Json& out, JsonParseError& error) {
    Parser parser(text, error);
    return parser.parse(out);
}

}  // namespace sr
