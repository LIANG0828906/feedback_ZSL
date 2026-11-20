#include "json.hpp"

#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <limits>
#include <sstream>
#include <stdexcept>

namespace fill::json {

const Value* Value::find(const std::string& key) const {
    if (type != Type::Object) {
        return nullptr;
    }
    const auto it = object.find(key);
    return it == object.end() ? nullptr : &it->second;
}

namespace {

class Parser {
public:
    explicit Parser(const std::string& text) : text_(text) {}

    ParseResult run() {
        ParseResult result;
        try {
            skipWhitespace();
            result.value = parseValue();
            skipWhitespace();
            if (position_ != text_.size()) {
                fail("trailing content");
            }
        } catch (const std::runtime_error& e) {
            result.error.message = e.what();
            result.error.offset = position_;
        }
        return result;
    }

private:
    const std::string& text_;
    std::size_t position_ = 0;

    [[noreturn]] void fail(const std::string& message) const {
        throw std::runtime_error(message + " at byte " + std::to_string(position_));
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
            if (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
                ++position_;
            } else {
                break;
            }
        }
    }

    void expect(char expected) {
        const char actual = take();
        if (actual != expected) {
            fail(std::string("expected '") + expected + "', got '" + actual + "'");
        }
    }

    Value parseValue() {
        skipWhitespace();
        const char c = peek();
        if (c == '{') {
            return parseObject();
        }
        if (c == '[') {
            return parseArray();
        }
        if (c == '"') {
            Value v;
            v.type = Value::Type::String;
            v.string = parseString();
            return v;
        }
        if (c == 't') {
            consumeWord("true");
            Value v;
            v.type = Value::Type::Bool;
            v.boolean = true;
            return v;
        }
        if (c == 'f') {
            consumeWord("false");
            Value v;
            v.type = Value::Type::Bool;
            v.boolean = false;
            return v;
        }
        if (c == 'n') {
            consumeWord("null");
            return Value{};
        }
        if (c == 'N') {
            consumeWord("NaN");
            Value v;
            v.type = Value::Type::Number;
            v.number = std::numeric_limits<double>::quiet_NaN();
            return v;
        }
        if (c == 'I') {
            consumeWord("Infinity");
            Value v;
            v.type = Value::Type::Number;
            v.number = std::numeric_limits<double>::infinity();
            return v;
        }
        if (c == '-' && text_.compare(position_, 9, "-Infinity") == 0) {
            position_ += 9;
            Value v;
            v.type = Value::Type::Number;
            v.number = -std::numeric_limits<double>::infinity();
            return v;
        }
        if (c == '-' || (c >= '0' && c <= '9')) {
            return parseNumber();
        }
        fail("unexpected token");
    }

    void consumeWord(const char* word) {
        const std::size_t n = std::char_traits<char>::length(word);
        if (text_.compare(position_, n, word) != 0) {
            fail(std::string("expected ") + word);
        }
        position_ += n;
    }

    Value parseObject() {
        Value v;
        v.type = Value::Type::Object;
        expect('{');
        skipWhitespace();
        if (peek() == '}') {
            take();
            return v;
        }
        while (true) {
            skipWhitespace();
            if (peek() != '"') {
                fail("expected object key");
            }
            std::string key = parseString();
            skipWhitespace();
            expect(':');
            Value item = parseValue();
            if (v.object.find(key) != v.object.end()) {
                fail("duplicate object key");
            }
            v.object.emplace(std::move(key), std::move(item));
            skipWhitespace();
            const char c = take();
            if (c == '}') {
                break;
            }
            if (c != ',') {
                fail("expected ',' or '}'");
            }
        }
        return v;
    }

    Value parseArray() {
        Value v;
        v.type = Value::Type::Array;
        expect('[');
        skipWhitespace();
        if (peek() == ']') {
            take();
            return v;
        }
        while (true) {
            v.array.push_back(parseValue());
            skipWhitespace();
            const char c = take();
            if (c == ']') {
                break;
            }
            if (c != ',') {
                fail("expected ',' or ']'");
            }
        }
        return v;
    }

    static void appendUtf8(std::string& output, unsigned codepoint) {
        if (codepoint <= 0x7F) {
            output.push_back(static_cast<char>(codepoint));
        } else if (codepoint <= 0x7FF) {
            output.push_back(static_cast<char>(0xC0 | (codepoint >> 6)));
            output.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
        } else if (codepoint <= 0xFFFF) {
            output.push_back(static_cast<char>(0xE0 | (codepoint >> 12)));
            output.push_back(static_cast<char>(0x80 | ((codepoint >> 6) & 0x3F)));
            output.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
        } else {
            output.push_back(static_cast<char>(0xF0 | (codepoint >> 18)));
            output.push_back(static_cast<char>(0x80 | ((codepoint >> 12) & 0x3F)));
            output.push_back(static_cast<char>(0x80 | ((codepoint >> 6) & 0x3F)));
            output.push_back(static_cast<char>(0x80 | (codepoint & 0x3F)));
        }
    }

    unsigned parseHex4() {
        unsigned value = 0;
        for (int i = 0; i < 4; ++i) {
            const char c = take();
            value <<= 4U;
            if (c >= '0' && c <= '9') {
                value |= static_cast<unsigned>(c - '0');
            } else if (c >= 'a' && c <= 'f') {
                value |= static_cast<unsigned>(c - 'a' + 10);
            } else if (c >= 'A' && c <= 'F') {
                value |= static_cast<unsigned>(c - 'A' + 10);
            } else {
                fail("invalid unicode escape");
            }
        }
        return value;
    }

    std::string parseString() {
        expect('"');
        std::string output;
        while (true) {
            if (position_ >= text_.size()) {
                fail("unterminated string");
            }
            const char c = take();
            if (c == '"') {
                break;
            }
            if (static_cast<unsigned char>(c) < 0x20) {
                fail("control character in string");
            }
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
                    unsigned cp = parseHex4();
                    if (cp >= 0xD800 && cp <= 0xDBFF) {
                        if (take() != '\\' || take() != 'u') {
                            fail("missing low surrogate");
                        }
                        const unsigned low = parseHex4();
                        if (low < 0xDC00 || low > 0xDFFF) {
                            fail("invalid low surrogate");
                        }
                        cp = 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00);
                    } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
                        fail("unexpected low surrogate");
                    }
                    appendUtf8(output, cp);
                    break;
                }
                default: fail("invalid escape");
            }
        }
        return output;
    }

    Value parseNumber() {
        const std::size_t begin = position_;
        if (peek() == '-') {
            ++position_;
        }
        if (peek() == '0') {
            ++position_;
        } else {
            if (peek() < '1' || peek() > '9') {
                fail("invalid number");
            }
            while (peek() >= '0' && peek() <= '9') {
                ++position_;
            }
        }
        if (peek() == '.') {
            ++position_;
            if (peek() < '0' || peek() > '9') {
                fail("invalid fraction");
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
            if (peek() < '0' || peek() > '9') {
                fail("invalid exponent");
            }
            while (peek() >= '0' && peek() <= '9') {
                ++position_;
            }
        }
        const std::string token = text_.substr(begin, position_ - begin);
        errno = 0;
        char* end = nullptr;
        const double value = std::strtod(token.c_str(), &end);
        if (end == nullptr || *end != '\0' || errno == ERANGE) {
            fail("number out of range");
        }
        Value v;
        v.type = Value::Type::Number;
        v.number = value;
        return v;
    }
};

}  // namespace

ParseResult parse(const std::string& text) {
    return Parser(text).run();
}

std::string escape(const std::string& value) {
    std::ostringstream out;
    out << '"';
    for (const unsigned char c : value) {
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
                    out << "\\u00" << hex[(c >> 4U) & 0xF] << hex[c & 0xF];
                } else {
                    out << static_cast<char>(c);
                }
                break;
        }
    }
    out << '"';
    return out.str();
}

}  // namespace fill::json
