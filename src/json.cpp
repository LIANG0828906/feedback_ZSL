#include "json.hpp"

#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <limits>
#include <sstream>
#include <string_view>

namespace sj {
namespace {

void appendUtf8(std::string& out, unsigned codepoint) {
    if (codepoint <= 0x7Fu) {
        out.push_back(static_cast<char>(codepoint));
    } else if (codepoint <= 0x7FFu) {
        out.push_back(static_cast<char>(0xC0u | (codepoint >> 6u)));
        out.push_back(static_cast<char>(0x80u | (codepoint & 0x3Fu)));
    } else if (codepoint <= 0xFFFFu) {
        out.push_back(static_cast<char>(0xE0u | (codepoint >> 12u)));
        out.push_back(static_cast<char>(0x80u | ((codepoint >> 6u) & 0x3Fu)));
        out.push_back(static_cast<char>(0x80u | (codepoint & 0x3Fu)));
    } else {
        out.push_back(static_cast<char>(0xF0u | (codepoint >> 18u)));
        out.push_back(static_cast<char>(0x80u | ((codepoint >> 12u) & 0x3Fu)));
        out.push_back(static_cast<char>(0x80u | ((codepoint >> 6u) & 0x3Fu)));
        out.push_back(static_cast<char>(0x80u | (codepoint & 0x3Fu)));
    }
}

std::string quote(const std::string& text) {
    std::string out;
    out.push_back('"');
    for (unsigned char c : text) {
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
                char buf[8];
                std::snprintf(buf, sizeof(buf), "\\u%04x", static_cast<unsigned>(c));
                out += buf;
            } else {
                out.push_back(static_cast<char>(c));
            }
            break;
        }
    }
    out.push_back('"');
    return out;
}

std::string numberText(double value) {
    if (!std::isfinite(value)) {
        return "null";
    }
    if (value == 0.0) {
        return "0";
    }
    if (std::floor(value) == value && std::fabs(value) < 9.0e15) {
        char buf[64];
        std::snprintf(buf, sizeof(buf), "%.0f", value);
        return buf;
    }
    char buf[64];
    std::snprintf(buf, sizeof(buf), "%.15g", value);
    return buf;
}

class Parser {
public:
    explicit Parser(const std::string& text) : text_(text) {}

    Value parseDocument(std::string& error) {
        skipSpace();
        Value value = parseValue();
        skipSpace();
        if (error_.empty() && pos_ != text_.size()) {
            fail("unexpected trailing data");
        }
        if (!error_.empty()) {
            error = error_;
            return Value();
        }
        return value;
    }

private:
    const std::string& text_;
    std::size_t pos_ = 0;
    std::string error_;

    void fail(const std::string& message) {
        if (!error_.empty()) {
            return;
        }
        std::size_t line = 1;
        std::size_t column = 1;
        for (std::size_t i = 0; i < pos_ && i < text_.size(); ++i) {
            if (text_[i] == '\n') {
                ++line;
                column = 1;
            } else {
                ++column;
            }
        }
        std::ostringstream out;
        out << "JSON parse error at line " << line << ", column " << column << ": " << message;
        error_ = out.str();
    }

    bool atEnd() const { return pos_ >= text_.size(); }
    char peek() const { return atEnd() ? '\0' : text_[pos_]; }
    char take() {
        if (atEnd()) {
            fail("unexpected end of input");
            return '\0';
        }
        return text_[pos_++];
    }
    void skipSpace() {
        while (!atEnd()) {
            char c = text_[pos_];
            if (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
                ++pos_;
            } else {
                break;
            }
        }
    }

    void expect(char c) {
        if (take() != c) {
            fail(std::string("expected '") + c + "'");
        }
    }

    bool consumeLiteral(std::string_view literal) {
        if (text_.compare(pos_, literal.size(), literal) == 0) {
            pos_ += literal.size();
            return true;
        }
        return false;
    }

    Value parseValue() {
        skipSpace();
        if (atEnd()) {
            fail("expected a value");
            return Value();
        }
        char c = peek();
        if (c == '{') return parseObject();
        if (c == '[') return parseArray();
        if (c == '"') return Value(parseString());
        if (c == 't') {
            if (consumeLiteral("true")) return Value(true);
            fail("invalid literal");
            return Value();
        }
        if (c == 'f') {
            if (consumeLiteral("false")) return Value(false);
            fail("invalid literal");
            return Value();
        }
        if (c == 'n') {
            if (consumeLiteral("null")) return Value();
            fail("invalid literal");
            return Value();
        }
        if (c == '-' || (c >= '0' && c <= '9')) return parseNumber();
        fail("unexpected character");
        return Value();
    }

    Value parseObject() {
        Value value = Value::object();
        expect('{');
        skipSpace();
        if (peek() == '}') {
            take();
            return value;
        }
        while (error_.empty()) {
            skipSpace();
            if (peek() != '"') {
                fail("expected object key");
                break;
            }
            std::string key = parseString();
            skipSpace();
            expect(':');
            Value item = parseValue();
            value.asObject()[key] = std::move(item);
            skipSpace();
            char c = take();
            if (c == '}') break;
            if (c != ',') {
                fail("expected ',' or '}'");
                break;
            }
        }
        return value;
    }

    Value parseArray() {
        Value value = Value::array();
        expect('[');
        skipSpace();
        if (peek() == ']') {
            take();
            return value;
        }
        while (error_.empty()) {
            value.asArray().push_back(parseValue());
            skipSpace();
            char c = take();
            if (c == ']') break;
            if (c != ',') {
                fail("expected ',' or ']'");
                break;
            }
        }
        return value;
    }

    static int hexDigit(char c) {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        return -1;
    }

    unsigned parseHex4() {
        unsigned result = 0;
        for (int i = 0; i < 4; ++i) {
            int digit = hexDigit(take());
            if (digit < 0) {
                fail("invalid unicode escape");
                return 0;
            }
            result = (result << 4u) | static_cast<unsigned>(digit);
        }
        return result;
    }

    std::string parseString() {
        expect('"');
        std::string out;
        while (error_.empty()) {
            char c = take();
            if (c == '"') break;
            if (c == '\\') {
                char esc = take();
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
                    unsigned cp = parseHex4();
                    if (cp >= 0xD800u && cp <= 0xDBFFu) {
                        if (take() != '\\' || take() != 'u') {
                            fail("missing low surrogate");
                            break;
                        }
                        unsigned low = parseHex4();
                        if (low < 0xDC00u || low > 0xDFFFu) {
                            fail("invalid low surrogate");
                            break;
                        }
                        cp = 0x10000u + ((cp - 0xD800u) << 10u) + (low - 0xDC00u);
                    } else if (cp >= 0xDC00u && cp <= 0xDFFFu) {
                        fail("unexpected low surrogate");
                        break;
                    }
                    appendUtf8(out, cp);
                    break;
                }
                default: fail("invalid escape"); break;
                }
            } else {
                if (static_cast<unsigned char>(c) < 0x20u) {
                    fail("control character in string");
                    break;
                }
                out.push_back(c);
            }
        }
        return out;
    }

    Value parseNumber() {
        std::size_t begin = pos_;
        if (peek() == '-') ++pos_;
        if (peek() == '0') {
            ++pos_;
        } else if (peek() >= '1' && peek() <= '9') {
            while (peek() >= '0' && peek() <= '9') ++pos_;
        } else {
            fail("invalid number");
            return Value();
        }
        if (peek() == '.') {
            ++pos_;
            if (!(peek() >= '0' && peek() <= '9')) {
                fail("invalid fractional part");
                return Value();
            }
            while (peek() >= '0' && peek() <= '9') ++pos_;
        }
        if (peek() == 'e' || peek() == 'E') {
            ++pos_;
            if (peek() == '+' || peek() == '-') ++pos_;
            if (!(peek() >= '0' && peek() <= '9')) {
                fail("invalid exponent");
                return Value();
            }
            while (peek() >= '0' && peek() <= '9') ++pos_;
        }
        std::string token = text_.substr(begin, pos_ - begin);
        char* end = nullptr;
        double number = std::strtod(token.c_str(), &end);
        if (end == nullptr || *end != '\0' || !std::isfinite(number)) {
            fail("number out of range");
            return Value();
        }
        return Value(number);
    }
};

} // namespace

Value Value::parse(const std::string& text, std::string& error) {
    error.clear();
    Parser parser(text);
    return parser.parseDocument(error);
}

void Value::dumpTo(std::string& out, int indent, int depth) const {
    switch (type_) {
    case Type::Null:
        out += "null";
        break;
    case Type::Bool:
        out += bool_ ? "true" : "false";
        break;
    case Type::Number:
        out += numberText(number_);
        break;
    case Type::String:
        out += quote(string_);
        break;
    case Type::Array: {
        out.push_back('[');
        for (std::size_t i = 0; i < array_.size(); ++i) {
            if (i != 0) out.push_back(',');
            if (indent >= 0) {
                out.push_back('\n');
                out.append(static_cast<std::size_t>((depth + 1) * indent), ' ');
            }
            array_[i].dumpTo(out, indent, depth + 1);
        }
        if (indent >= 0 && !array_.empty()) {
            out.push_back('\n');
            out.append(static_cast<std::size_t>(depth * indent), ' ');
        }
        out.push_back(']');
        break;
    }
    case Type::Object: {
        out.push_back('{');
        std::size_t i = 0;
        for (const auto& pair : object_) {
            if (i++ != 0) out.push_back(',');
            if (indent >= 0) {
                out.push_back('\n');
                out.append(static_cast<std::size_t>((depth + 1) * indent), ' ');
            }
            out += quote(pair.first);
            out.push_back(':');
            if (indent >= 0) out.push_back(' ');
            pair.second.dumpTo(out, indent, depth + 1);
        }
        if (indent >= 0 && !object_.empty()) {
            out.push_back('\n');
            out.append(static_cast<std::size_t>(depth * indent), ' ');
        }
        out.push_back('}');
        break;
    }
    }
}

std::string Value::dump(int indent) const {
    std::string out;
    dumpTo(out, indent, 0);
    return out;
}

} // namespace sj

