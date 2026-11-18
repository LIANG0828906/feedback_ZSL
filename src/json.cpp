#include "json.h"

#include <cmath>
#include <cstdlib>
#include <limits>
#include <sstream>

namespace caisson::json {
namespace {

class Parser {
public:
    explicit Parser(const std::string& text) : text_(text) {}

    Value parseDocument() {
        skipWhitespace();
        Value value = parseValue();
        skipWhitespace();
        if (position_ != text_.size()) {
            fail("trailing characters after JSON document");
        }
        return value;
    }

private:
    const std::string& text_;
    std::size_t position_ = 0;
    int line_ = 1;
    int column_ = 1;

    [[noreturn]] void fail(const std::string& message) const {
        throw ParseError(message, line_, column_);
    }

    char peek() const {
        if (position_ >= text_.size()) return '\0';
        return text_[position_];
    }

    char take() {
        if (position_ >= text_.size()) fail("unexpected end of input");
        const char c = text_[position_++];
        if (c == '\n') {
            ++line_;
            column_ = 1;
        } else {
            ++column_;
        }
        return c;
    }

    void skipWhitespace() {
        while (position_ < text_.size()) {
            const char c = text_[position_];
            if (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
                take();
            } else {
                break;
            }
        }
    }

    void expect(char expected) {
        if (take() != expected) fail(std::string("expected '") + expected + "'");
    }

    Value parseValue() {
        skipWhitespace();
        const char c = peek();
        if (c == '{') return parseObject();
        if (c == '[') return parseArray();
        if (c == '"') {
            Value value;
            value.type = Value::Type::String;
            value.string = parseString();
            return value;
        }
        if (c == 't') {
            parseLiteral("true");
            Value value;
            value.type = Value::Type::Boolean;
            value.boolean = true;
            return value;
        }
        if (c == 'f') {
            parseLiteral("false");
            Value value;
            value.type = Value::Type::Boolean;
            value.boolean = false;
            return value;
        }
        if (c == 'n') {
            parseLiteral("null");
            return Value{};
        }
        if (c == '-' || (c >= '0' && c <= '9')) return parseNumber();
        fail("unexpected character");
    }

    void parseLiteral(const char* literal) {
        for (const char* p = literal; *p != '\0'; ++p) {
            if (take() != *p) fail("invalid literal");
        }
    }

    Value parseObject() {
        expect('{');
        Value value;
        value.type = Value::Type::Object;
        skipWhitespace();
        if (peek() == '}') {
            take();
            return value;
        }
        while (true) {
            skipWhitespace();
            if (peek() != '"') fail("object key must be a string");
            const std::string key = parseString();
            skipWhitespace();
            expect(':');
            Value child = parseValue();
            if (value.object.count(key) != 0) fail("duplicate object key");
            value.object.emplace(key, std::move(child));
            skipWhitespace();
            const char c = take();
            if (c == '}') break;
            if (c != ',') fail("expected ',' or '}'");
        }
        return value;
    }

    Value parseArray() {
        expect('[');
        Value value;
        value.type = Value::Type::Array;
        skipWhitespace();
        if (peek() == ']') {
            take();
            return value;
        }
        while (true) {
            value.array.push_back(parseValue());
            skipWhitespace();
            const char c = take();
            if (c == ']') break;
            if (c != ',') fail("expected ',' or ']'");
        }
        return value;
    }

    std::string parseString() {
        expect('"');
        std::string output;
        while (true) {
            const char c = take();
            if (c == '"') break;
            if (static_cast<unsigned char>(c) < 0x20) fail("control character in string");
            if (c != '\\') {
                output.push_back(c);
                continue;
            }
            const char escaped = take();
            switch (escaped) {
                case '"': output.push_back('"'); break;
                case '\\': output.push_back('\\'); break;
                case '/': output.push_back('/'); break;
                case 'b': output.push_back('\b'); break;
                case 'f': output.push_back('\f'); break;
                case 'n': output.push_back('\n'); break;
                case 'r': output.push_back('\r'); break;
                case 't': output.push_back('\t'); break;
                case 'u': {
                    const unsigned codepoint = parseHex4();
                    if (codepoint <= 0x7F) {
                        output.push_back(static_cast<char>(codepoint));
                    } else if (codepoint <= 0x7FF) {
                        output.push_back(static_cast<char>(0xC0 | (codepoint >> 6)));
                        output.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
                    } else {
                        output.push_back(static_cast<char>(0xE0 | (codepoint >> 12)));
                        output.push_back(static_cast<char>(0x80 | ((codepoint >> 6) & 0x3F)));
                        output.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
                    }
                    break;
                }
                default: fail("invalid escape sequence");
            }
        }
        return output;
    }

    unsigned parseHex4() {
        unsigned value = 0;
        for (int i = 0; i < 4; ++i) {
            const char c = take();
            value <<= 4;
            if (c >= '0' && c <= '9') value |= static_cast<unsigned>(c - '0');
            else if (c >= 'a' && c <= 'f') value |= static_cast<unsigned>(c - 'a' + 10);
            else if (c >= 'A' && c <= 'F') value |= static_cast<unsigned>(c - 'A' + 10);
            else fail("invalid unicode escape");
        }
        return value;
    }

    Value parseNumber() {
        const std::size_t start = position_;
        if (peek() == '-') take();
        if (peek() == '0') {
            take();
        } else {
            if (peek() < '1' || peek() > '9') fail("invalid number");
            while (peek() >= '0' && peek() <= '9') take();
        }
        if (peek() == '.') {
            take();
            if (peek() < '0' || peek() > '9') fail("invalid fraction");
            while (peek() >= '0' && peek() <= '9') take();
        }
        if (peek() == 'e' || peek() == 'E') {
            take();
            if (peek() == '+' || peek() == '-') take();
            if (peek() < '0' || peek() > '9') fail("invalid exponent");
            while (peek() >= '0' && peek() <= '9') take();
        }
        const std::string token = text_.substr(start, position_ - start);
        char* end = nullptr;
        const double number = std::strtod(token.c_str(), &end);
        if (end == token.c_str() || *end != '\0' || !std::isfinite(number)) {
            fail("number is not finite");
        }
        Value value;
        value.type = Value::Type::Number;
        value.number = number;
        return value;
    }
};

}  // namespace

Value parse(const std::string& text) {
    Parser parser(text);
    return parser.parseDocument();
}

std::string escape(const std::string& input) {
    std::ostringstream out;
    for (const unsigned char c : input) {
        switch (c) {
            case '"': out << "\\\""; break;
            case '\\': out << "\\\\"; break;
            case '\b': out << "\\b"; break;
            case '\f': out << "\\f"; break;
            case '\n': out << "\\n"; break;
            case '\r': out << "\\r"; break;
            case '\t': out << "\\t"; break;
            default:
                if (c < 0x20) {
                    static const char* hex = "0123456789abcdef";
                    out << "\\u00" << hex[(c >> 4) & 0xF] << hex[c & 0xF];
                } else {
                    out << static_cast<char>(c);
                }
        }
    }
    return out.str();
}

}  // namespace caisson::json

