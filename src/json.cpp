#include "json.hpp"

#include <charconv>
#include <cmath>
#include <cstdint>
#include <cstdlib>
#include <limits>
#include <stdexcept>
#include <string>

namespace mosaic {

JsonValue JsonValue::makeBool(bool value) {
    JsonValue result;
    result.type_ = Type::Bool;
    result.bool_ = value;
    return result;
}

JsonValue JsonValue::makeNumber(double value) {
    JsonValue result;
    result.type_ = Type::Number;
    result.number_ = value;
    return result;
}

JsonValue JsonValue::makeString(std::string value) {
    JsonValue result;
    result.type_ = Type::String;
    result.string_ = std::move(value);
    return result;
}

JsonValue JsonValue::makeArray(std::vector<JsonValue> value) {
    JsonValue result;
    result.type_ = Type::Array;
    result.array_ = std::move(value);
    return result;
}

JsonValue JsonValue::makeObject(std::vector<std::pair<std::string, JsonValue>> value) {
    JsonValue result;
    result.type_ = Type::Object;
    result.object_ = std::move(value);
    return result;
}

const JsonValue* JsonValue::find(std::string_view key) const {
    if (type_ != Type::Object) {
        return nullptr;
    }
    for (const auto& [name, value] : object_) {
        if (name == key) {
            return &value;
        }
    }
    return nullptr;
}

std::size_t JsonValue::size() const {
    if (type_ == Type::Array) {
        return array_.size();
    }
    if (type_ == Type::Object) {
        return object_.size();
    }
    return 0;
}

namespace {

class Parser {
public:
    explicit Parser(std::string_view text) : text_(text) {}

    JsonParseResult parse() {
        JsonParseResult result;
        try {
            skipWhitespace();
            JsonValue value = parseValue();
            skipWhitespace();
            if (position_ != text_.size()) {
                fail("unexpected trailing character");
            }
            result.value = std::move(value);
        } catch (const ParseFailure& failure) {
            JsonParseError error;
            error.offset = failure.offset;
            error.message = failure.message;
            computeLineColumn(failure.offset, error.line, error.column);
            result.error = std::move(error);
        }
        return result;
    }

private:
    struct ParseFailure {
        std::size_t offset;
        std::string message;
    };

    [[noreturn]] void fail(const std::string& message) const {
        throw ParseFailure{position_, message};
    }

    char peek() const {
        return position_ < text_.size() ? text_[position_] : '\0';
    }

    char take() {
        if (position_ >= text_.size()) {
            fail("unexpected end of input");
        }
        return text_[position_++];
    }

    void skipWhitespace() {
        while (position_ < text_.size()) {
            const char c = text_[position_];
            if (c != ' ' && c != '\t' && c != '\n' && c != '\r') {
                break;
            }
            ++position_;
        }
    }

    JsonValue parseValue() {
        skipWhitespace();
        switch (peek()) {
        case '{':
            return parseObject();
        case '[':
            return parseArray();
        case '"':
            return JsonValue::makeString(parseString());
        case 't':
            expectLiteral("true");
            return JsonValue::makeBool(true);
        case 'f':
            expectLiteral("false");
            return JsonValue::makeBool(false);
        case 'n':
            expectLiteral("null");
            return JsonValue{};
        default:
            if (peek() == '-' || (peek() >= '0' && peek() <= '9')) {
                return parseNumber();
            }
            fail("expected a JSON value");
        }
    }

    void expectLiteral(std::string_view literal) {
        if (text_.substr(position_, literal.size()) != literal) {
            fail("invalid literal");
        }
        position_ += literal.size();
    }

    JsonValue parseObject() {
        take();
        skipWhitespace();
        std::vector<std::pair<std::string, JsonValue>> members;
        if (peek() == '}') {
            take();
            return JsonValue::makeObject(std::move(members));
        }
        while (true) {
            skipWhitespace();
            if (peek() != '"') {
                fail("expected an object key");
            }
            std::string key = parseString();
            skipWhitespace();
            if (take() != ':') {
                fail("expected ':' after object key");
            }
            JsonValue value = parseValue();
            bool replaced = false;
            for (auto& [existingKey, existingValue] : members) {
                if (existingKey == key) {
                    existingValue = std::move(value);
                    replaced = true;
                    break;
                }
            }
            if (!replaced) {
                members.emplace_back(std::move(key), std::move(value));
            }
            skipWhitespace();
            const char separator = take();
            if (separator == '}') {
                break;
            }
            if (separator != ',') {
                fail("expected ',' or '}' in object");
            }
        }
        return JsonValue::makeObject(std::move(members));
    }

    JsonValue parseArray() {
        take();
        skipWhitespace();
        std::vector<JsonValue> values;
        if (peek() == ']') {
            take();
            return JsonValue::makeArray(std::move(values));
        }
        while (true) {
            values.push_back(parseValue());
            skipWhitespace();
            const char separator = take();
            if (separator == ']') {
                break;
            }
            if (separator != ',') {
                fail("expected ',' or ']' in array");
            }
        }
        return JsonValue::makeArray(std::move(values));
    }

    static void appendUtf8(std::string& out, uint32_t codepoint) {
        if (codepoint <= 0x7FU) {
            out.push_back(static_cast<char>(codepoint));
        } else if (codepoint <= 0x7FFU) {
            out.push_back(static_cast<char>(0xC0U | (codepoint >> 6U)));
            out.push_back(static_cast<char>(0x80U | (codepoint & 0x3FU)));
        } else if (codepoint <= 0xFFFFU) {
            out.push_back(static_cast<char>(0xE0U | (codepoint >> 12U)));
            out.push_back(static_cast<char>(0x80U | ((codepoint >> 6U) & 0x3FU)));
            out.push_back(static_cast<char>(0x80U | (codepoint & 0x3FU)));
        } else {
            out.push_back(static_cast<char>(0xF0U | (codepoint >> 18U)));
            out.push_back(static_cast<char>(0x80U | ((codepoint >> 12U) & 0x3FU)));
            out.push_back(static_cast<char>(0x80U | ((codepoint >> 6U) & 0x3FU)));
            out.push_back(static_cast<char>(0x80U | (codepoint & 0x3FU)));
        }
    }

    uint32_t parseHex4() {
        uint32_t value = 0;
        for (int i = 0; i < 4; ++i) {
            const char c = take();
            value <<= 4U;
            if (c >= '0' && c <= '9') {
                value |= static_cast<uint32_t>(c - '0');
            } else if (c >= 'a' && c <= 'f') {
                value |= static_cast<uint32_t>(c - 'a' + 10);
            } else if (c >= 'A' && c <= 'F') {
                value |= static_cast<uint32_t>(c - 'A' + 10);
            } else {
                fail("invalid Unicode escape");
            }
        }
        return value;
    }

    std::string parseString() {
        take();
        std::string out;
        while (true) {
            if (position_ >= text_.size()) {
                fail("unterminated string");
            }
            const char c = take();
            if (c == '"') {
                break;
            }
            if (static_cast<unsigned char>(c) < 0x20U) {
                fail("control character in string");
            }
            if (c != '\\') {
                out.push_back(c);
                continue;
            }
            const char escape = take();
            switch (escape) {
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
                uint32_t codepoint = parseHex4();
                if (codepoint >= 0xD800U && codepoint <= 0xDBFFU) {
                    if (take() != '\\' || take() != 'u') {
                        fail("missing low surrogate");
                    }
                    const uint32_t low = parseHex4();
                    if (low < 0xDC00U || low > 0xDFFFU) {
                        fail("invalid low surrogate");
                    }
                    codepoint = 0x10000U + ((codepoint - 0xD800U) << 10U) + (low - 0xDC00U);
                } else if (codepoint >= 0xDC00U && codepoint <= 0xDFFFU) {
                    fail("unexpected low surrogate");
                }
                appendUtf8(out, codepoint);
                break;
            }
            default:
                fail("invalid escape sequence");
            }
        }
        return out;
    }

    JsonValue parseNumber() {
        const std::size_t start = position_;
        if (peek() == '-') {
            ++position_;
        }
        if (peek() == '0') {
            ++position_;
        } else if (peek() >= '1' && peek() <= '9') {
            while (peek() >= '0' && peek() <= '9') {
                ++position_;
            }
        } else {
            fail("invalid number");
        }
        if (peek() == '.') {
            ++position_;
            if (!(peek() >= '0' && peek() <= '9')) {
                fail("expected digits after decimal point");
            }
            while (peek() >= '0' && peek() <= '9') {
                ++position_;
            }
        }
        if (peek() == 'e' || peek() == 'E') {
            ++position_;
            if (peek() == '+' || peek() == '-') {
                ++position_;
            }
            if (!(peek() >= '0' && peek() <= '9')) {
                fail("expected exponent digits");
            }
            while (peek() >= '0' && peek() <= '9') {
                ++position_;
            }
        }
        const std::string_view token = text_.substr(start, position_ - start);
        double value = 0.0;
#if defined(__cpp_lib_to_chars)
        const auto conversion = std::from_chars(token.data(), token.data() + token.size(), value);
        if (conversion.ec != std::errc{} || conversion.ptr != token.data() + token.size()) {
            fail("number out of range");
        }
#else
        const std::string temporary(token);
        char* end = nullptr;
        value = std::strtod(temporary.c_str(), &end);
        if (end == nullptr || *end != '\0') {
            fail("number out of range");
        }
#endif
        if (!std::isfinite(value)) {
            fail("number must be finite");
        }
        return JsonValue::makeNumber(value);
    }

    void computeLineColumn(std::size_t offset, std::size_t& line, std::size_t& column) const {
        line = 1;
        column = 1;
        const std::size_t limit = std::min(offset, text_.size());
        for (std::size_t i = 0; i < limit; ++i) {
            if (text_[i] == '\n') {
                ++line;
                column = 1;
            } else {
                ++column;
            }
        }
    }

    std::string_view text_;
    std::size_t position_ = 0;
};

}  // namespace

JsonParseResult parseJson(std::string_view text) {
    return Parser(text).parse();
}

std::string escapeJsonString(std::string_view text) {
    std::string out;
    out.reserve(text.size() + 8);
    static constexpr char digits[] = "0123456789abcdef";
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
                out += "\\u00";
                out.push_back(digits[c >> 4U]);
                out.push_back(digits[c & 0xFU]);
            } else {
                out.push_back(static_cast<char>(c));
            }
        }
    }
    return out;
}

}  // namespace mosaic
