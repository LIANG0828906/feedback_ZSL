#include "font_atlas/json.hpp"

#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <fstream>
#include <iomanip>
#include <limits>
#include <sstream>
#include <stdexcept>

namespace font_atlas {
namespace {

Diagnostic parseError(const std::string& text, std::size_t position,
                      const std::string& message) {
    std::size_t line = 1;
    std::size_t column = 1;
    for (std::size_t i = 0; i < position && i < text.size(); ++i) {
        if (text[i] == '\n') {
            ++line;
            column = 1;
        } else {
            ++column;
        }
    }
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::InvalidArgument;
    diagnostic.message = message + " at line " + std::to_string(line) + ", column " +
                         std::to_string(column);
    return diagnostic;
}

void appendUtf8(std::string& output, std::uint32_t codePoint) {
    if (codePoint <= 0x7fu) {
        output.push_back(static_cast<char>(codePoint));
    } else if (codePoint <= 0x7ffu) {
        output.push_back(static_cast<char>(0xc0u | (codePoint >> 6u)));
        output.push_back(static_cast<char>(0x80u | (codePoint & 0x3fu)));
    } else if (codePoint <= 0xffffu) {
        output.push_back(static_cast<char>(0xe0u | (codePoint >> 12u)));
        output.push_back(static_cast<char>(0x80u | ((codePoint >> 6u) & 0x3fu)));
        output.push_back(static_cast<char>(0x80u | (codePoint & 0x3fu)));
    } else {
        output.push_back(static_cast<char>(0xf0u | (codePoint >> 18u)));
        output.push_back(static_cast<char>(0x80u | ((codePoint >> 12u) & 0x3fu)));
        output.push_back(static_cast<char>(0x80u | ((codePoint >> 6u) & 0x3fu)));
        output.push_back(static_cast<char>(0x80u | (codePoint & 0x3fu)));
    }
}

int hexValue(char c) {
    if (c >= '0' && c <= '9') {
        return c - '0';
    }
    if (c >= 'a' && c <= 'f') {
        return 10 + c - 'a';
    }
    if (c >= 'A' && c <= 'F') {
        return 10 + c - 'A';
    }
    return -1;
}

void dumpString(std::ostringstream& output, const std::string& value) {
    output << '"';
    for (unsigned char c : value) {
        switch (c) {
            case '"':
                output << "\\\"";
                break;
            case '\\':
                output << "\\\\";
                break;
            case '\b':
                output << "\\b";
                break;
            case '\f':
                output << "\\f";
                break;
            case '\n':
                output << "\\n";
                break;
            case '\r':
                output << "\\r";
                break;
            case '\t':
                output << "\\t";
                break;
            default:
                if (c < 0x20u) {
                    output << "\\u" << std::hex << std::setw(4) << std::setfill('0')
                           << static_cast<unsigned>(c) << std::dec << std::setfill(' ');
                } else {
                    output << static_cast<char>(c);
                }
                break;
        }
    }
    output << '"';
}

void dumpValue(std::ostringstream& output, const Json& value, int indent, int depth) {
    const auto newline = [&](int targetDepth) {
        if (indent >= 0) {
            output << '\n' << std::string(static_cast<std::size_t>(indent * targetDepth), ' ');
        }
    };

    switch (value.type()) {
        case Json::Type::Null:
            output << "null";
            break;
        case Json::Type::Bool:
            output << (value.asBool() ? "true" : "false");
            break;
        case Json::Type::Number: {
            const double number = value.asNumber();
            if (std::isfinite(number) && std::floor(number) == number &&
                std::abs(number) < 9.0e15) {
                output << std::fixed << std::setprecision(0) << number << std::defaultfloat;
            } else {
                output << std::setprecision(17) << number;
            }
            break;
        }
        case Json::Type::String:
            dumpString(output, value.asString());
            break;
        case Json::Type::Array: {
            output << '[';
            const auto& items = value.asArray();
            for (std::size_t i = 0; i < items.size(); ++i) {
                if (i > 0) {
                    output << ',';
                }
                newline(depth + 1);
                dumpValue(output, items[i], indent, depth + 1);
            }
            if (!items.empty()) {
                newline(depth);
            }
            output << ']';
            break;
        }
        case Json::Type::Object: {
            output << '{';
            const auto& items = value.asObject();
            std::size_t index = 0;
            for (const auto& item : items) {
                if (index++ > 0) {
                    output << ',';
                }
                newline(depth + 1);
                dumpString(output, item.first);
                output << (indent >= 0 ? ": " : ":");
                dumpValue(output, item.second, indent, depth + 1);
            }
            if (!items.empty()) {
                newline(depth);
            }
            output << '}';
            break;
        }
    }
}

class Parser {
public:
    explicit Parser(const std::string& text) : text_(text), position_(0) {}

    Result<Json> parse() {
        skipWhitespace();
        auto value = parseValue();
        if (!value.ok()) {
            return value;
        }
        skipWhitespace();
        if (position_ != text_.size()) {
            return fail("trailing data");
        }
        return value;
    }

private:
    Result<Json> fail(const std::string& message) {
        return Result<Json>::failure(parseError(text_, position_, message));
    }

    void skipWhitespace() {
        while (position_ < text_.size()) {
            const char c = text_[position_];
            if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
                ++position_;
            } else {
                break;
            }
        }
    }

    bool consume(char expected) {
        if (position_ < text_.size() && text_[position_] == expected) {
            ++position_;
            return true;
        }
        return false;
    }

    Result<Json> parseValue() {
        if (position_ >= text_.size()) {
            return fail("unexpected end of input");
        }
        const char c = text_[position_];
        if (c == '{') {
            return parseObject();
        }
        if (c == '[') {
            return parseArray();
        }
        if (c == '"') {
            return parseStringValue();
        }
        if (c == 't' && text_.compare(position_, 4, "true") == 0) {
            position_ += 4;
            return Result<Json>::success(Json(true));
        }
        if (c == 'f' && text_.compare(position_, 5, "false") == 0) {
            position_ += 5;
            return Result<Json>::success(Json(false));
        }
        if (c == 'n' && text_.compare(position_, 4, "null") == 0) {
            position_ += 4;
            return Result<Json>::success(Json());
        }
        if (c == '-' || (c >= '0' && c <= '9')) {
            return parseNumber();
        }
        return fail("invalid JSON value");
    }

    Result<Json> parseObject() {
        ++position_;
        Json::Object object;
        skipWhitespace();
        if (consume('}')) {
            return Result<Json>::success(Json(std::move(object)));
        }
        while (true) {
            skipWhitespace();
            if (position_ >= text_.size() || text_[position_] != '"') {
                return fail("object key must be a string");
            }
            auto key = parseString();
            if (!key.ok()) {
                return Result<Json>::failure(key.error);
            }
            skipWhitespace();
            if (!consume(':')) {
                return fail("expected ':' after object key");
            }
            skipWhitespace();
            auto value = parseValue();
            if (!value.ok()) {
                return value;
            }
            if (!object.emplace(*key.value, std::move(*value.value)).second) {
                return fail("duplicate object key");
            }
            skipWhitespace();
            if (consume('}')) {
                break;
            }
            if (!consume(',')) {
                return fail("expected ',' or '}' in object");
            }
        }
        return Result<Json>::success(Json(std::move(object)));
    }

    Result<Json> parseArray() {
        ++position_;
        Json::Array array;
        skipWhitespace();
        if (consume(']')) {
            return Result<Json>::success(Json(std::move(array)));
        }
        while (true) {
            skipWhitespace();
            auto value = parseValue();
            if (!value.ok()) {
                return value;
            }
            array.push_back(std::move(*value.value));
            skipWhitespace();
            if (consume(']')) {
                break;
            }
            if (!consume(',')) {
                return fail("expected ',' or ']' in array");
            }
        }
        return Result<Json>::success(Json(std::move(array)));
    }

    Result<Json> parseStringValue() {
        auto value = parseString();
        if (!value.ok()) {
            return Result<Json>::failure(value.error);
        }
        return Result<Json>::success(Json(std::move(*value.value)));
    }

    Result<std::string> parseString() {
        if (!consume('"')) {
            return Result<std::string>::failure(
                parseError(text_, position_, "expected string"));
        }
        std::string result;
        while (position_ < text_.size()) {
            const unsigned char c = static_cast<unsigned char>(text_[position_++]);
            if (c == '"') {
                return Result<std::string>::success(std::move(result));
            }
            if (c < 0x20u) {
                return Result<std::string>::failure(
                    parseError(text_, position_ - 1, "control character in string"));
            }
            if (c != '\\') {
                result.push_back(static_cast<char>(c));
                continue;
            }
            if (position_ >= text_.size()) {
                return Result<std::string>::failure(
                    parseError(text_, position_, "truncated escape sequence"));
            }
            const char escaped = text_[position_++];
            switch (escaped) {
                case '"':
                case '\\':
                case '/':
                    result.push_back(escaped);
                    break;
                case 'b':
                    result.push_back('\b');
                    break;
                case 'f':
                    result.push_back('\f');
                    break;
                case 'n':
                    result.push_back('\n');
                    break;
                case 'r':
                    result.push_back('\r');
                    break;
                case 't':
                    result.push_back('\t');
                    break;
                case 'u': {
                    if (position_ + 4 > text_.size()) {
                        return Result<std::string>::failure(
                            parseError(text_, position_, "truncated unicode escape"));
                    }
                    std::uint32_t codePoint = 0;
                    for (int i = 0; i < 4; ++i) {
                        const int digit = hexValue(text_[position_++]);
                        if (digit < 0) {
                            return Result<std::string>::failure(
                                parseError(text_, position_ - 1, "invalid unicode escape"));
                        }
                        codePoint = (codePoint << 4u) | static_cast<std::uint32_t>(digit);
                    }
                    if (codePoint >= 0xd800u && codePoint <= 0xdbffu) {
                        if (position_ + 6 > text_.size() || text_[position_] != '\\' ||
                            text_[position_ + 1] != 'u') {
                            return Result<std::string>::failure(
                                parseError(text_, position_, "missing low surrogate"));
                        }
                        position_ += 2;
                        std::uint32_t low = 0;
                        for (int i = 0; i < 4; ++i) {
                            const int digit = hexValue(text_[position_++]);
                            if (digit < 0) {
                                return Result<std::string>::failure(
                                    parseError(text_, position_ - 1,
                                               "invalid low surrogate"));
                            }
                            low = (low << 4u) | static_cast<std::uint32_t>(digit);
                        }
                        if (low < 0xdc00u || low > 0xdfffu) {
                            return Result<std::string>::failure(
                                parseError(text_, position_, "invalid low surrogate"));
                        }
                        codePoint = 0x10000u + ((codePoint - 0xd800u) << 10u) +
                                    (low - 0xdc00u);
                    } else if (codePoint >= 0xdc00u && codePoint <= 0xdfffu) {
                        return Result<std::string>::failure(
                            parseError(text_, position_, "unexpected low surrogate"));
                    }
                    appendUtf8(result, codePoint);
                    break;
                }
                default:
                    return Result<std::string>::failure(
                        parseError(text_, position_ - 1, "invalid escape sequence"));
            }
        }
        return Result<std::string>::failure(
            parseError(text_, position_, "unterminated string"));
    }

    Result<Json> parseNumber() {
        const std::size_t start = position_;
        if (text_[position_] == '-') {
            ++position_;
        }
        if (position_ >= text_.size() || text_[position_] < '0' || text_[position_] > '9') {
            return fail("invalid number");
        }
        if (text_[position_] == '0') {
            ++position_;
        } else {
            while (position_ < text_.size() && text_[position_] >= '0' &&
                   text_[position_] <= '9') {
                ++position_;
            }
        }
        if (position_ < text_.size() && text_[position_] == '.') {
            ++position_;
            if (position_ >= text_.size() || text_[position_] < '0' || text_[position_] > '9') {
                return fail("invalid fraction");
            }
            while (position_ < text_.size() && text_[position_] >= '0' &&
                   text_[position_] <= '9') {
                ++position_;
            }
        }
        if (position_ < text_.size() &&
            (text_[position_] == 'e' || text_[position_] == 'E')) {
            ++position_;
            if (position_ < text_.size() &&
                (text_[position_] == '+' || text_[position_] == '-')) {
                ++position_;
            }
            if (position_ >= text_.size() || text_[position_] < '0' || text_[position_] > '9') {
                return fail("invalid exponent");
            }
            while (position_ < text_.size() && text_[position_] >= '0' &&
                   text_[position_] <= '9') {
                ++position_;
            }
        }
        const std::string token = text_.substr(start, position_ - start);
        errno = 0;
        char* end = nullptr;
        const double value = std::strtod(token.c_str(), &end);
        if (errno == ERANGE || end == nullptr || *end != '\0' || !std::isfinite(value)) {
            return fail("number out of range");
        }
        return Result<Json>::success(Json(value));
    }

    const std::string& text_;
    std::size_t position_;
};

}  // namespace

Json::Json() : type_(Type::Null) {}
Json::Json(bool value) : type_(Type::Bool), boolValue_(value) {}
Json::Json(double value) : type_(Type::Number), numberValue_(value) {}
Json::Json(int value) : type_(Type::Number), numberValue_(value) {}
Json::Json(std::int64_t value)
    : type_(Type::Number), numberValue_(static_cast<double>(value)) {}
Json::Json(std::uint64_t value)
    : type_(Type::Number), numberValue_(static_cast<double>(value)) {}
Json::Json(const char* value) : type_(Type::String), stringValue_(value == nullptr ? "" : value) {}
Json::Json(std::string value) : type_(Type::String), stringValue_(std::move(value)) {}
Json::Json(Array value) : type_(Type::Array), arrayValue_(std::move(value)) {}
Json::Json(Object value) : type_(Type::Object), objectValue_(std::move(value)) {}

Json Json::array() {
    return Json(Array{});
}

Json Json::object() {
    return Json(Object{});
}

Json Json::null() {
    return Json();
}

bool Json::asBool() const {
    if (type_ != Type::Bool) {
        throw std::logic_error("JSON value is not a boolean");
    }
    return boolValue_;
}

double Json::asNumber() const {
    if (type_ != Type::Number) {
        throw std::logic_error("JSON value is not a number");
    }
    return numberValue_;
}

std::int64_t Json::asInt64() const {
    const double value = asNumber();
    if (std::floor(value) != value) {
        throw std::logic_error("JSON number is not an integer");
    }
    if (value < static_cast<double>(std::numeric_limits<std::int64_t>::min()) ||
        value > static_cast<double>(std::numeric_limits<std::int64_t>::max())) {
        throw std::out_of_range("JSON integer out of range");
    }
    return static_cast<std::int64_t>(value);
}

int Json::asInt() const {
    const std::int64_t value = asInt64();
    if (value < std::numeric_limits<int>::min() || value > std::numeric_limits<int>::max()) {
        throw std::out_of_range("JSON integer out of int range");
    }
    return static_cast<int>(value);
}

std::uint64_t Json::asUInt64() const {
    const double value = asNumber();
    if (std::floor(value) != value) {
        throw std::logic_error("JSON number is not an integer");
    }
    if (value < 0.0 ||
        value > static_cast<double>(std::numeric_limits<std::uint64_t>::max())) {
        throw std::out_of_range("JSON unsigned integer out of range");
    }
    return static_cast<std::uint64_t>(value);
}

const std::string& Json::asString() const {
    if (type_ != Type::String) {
        throw std::logic_error("JSON value is not a string");
    }
    return stringValue_;
}

const Json::Array& Json::asArray() const {
    if (type_ != Type::Array) {
        throw std::logic_error("JSON value is not an array");
    }
    return arrayValue_;
}

Json::Array& Json::asArray() {
    if (type_ != Type::Array) {
        throw std::logic_error("JSON value is not an array");
    }
    return arrayValue_;
}

const Json::Object& Json::asObject() const {
    if (type_ != Type::Object) {
        throw std::logic_error("JSON value is not an object");
    }
    return objectValue_;
}

Json::Object& Json::asObject() {
    if (type_ != Type::Object) {
        throw std::logic_error("JSON value is not an object");
    }
    return objectValue_;
}

const Json* Json::find(const std::string& key) const {
    if (type_ != Type::Object) {
        return nullptr;
    }
    const auto iterator = objectValue_.find(key);
    return iterator == objectValue_.end() ? nullptr : &iterator->second;
}

Json* Json::find(const std::string& key) {
    if (type_ != Type::Object) {
        return nullptr;
    }
    const auto iterator = objectValue_.find(key);
    return iterator == objectValue_.end() ? nullptr : &iterator->second;
}

bool Json::contains(const std::string& key) const {
    return find(key) != nullptr;
}

const Json& Json::at(const std::string& key) const {
    const Json* value = find(key);
    if (value == nullptr) {
        throw std::out_of_range("missing JSON key: " + key);
    }
    return *value;
}

Json& Json::operator[](const std::string& key) {
    if (type_ != Type::Object) {
        type_ = Type::Object;
        objectValue_.clear();
    }
    return objectValue_[key];
}

void Json::erase(const std::string& key) {
    if (type_ == Type::Object) {
        objectValue_.erase(key);
    }
}

void Json::pushBack(Json value) {
    if (type_ != Type::Array) {
        type_ = Type::Array;
        arrayValue_.clear();
    }
    arrayValue_.push_back(std::move(value));
}

std::size_t Json::size() const {
    if (type_ == Type::Array) {
        return arrayValue_.size();
    }
    if (type_ == Type::Object) {
        return objectValue_.size();
    }
    if (type_ == Type::String) {
        return stringValue_.size();
    }
    return 0;
}

std::string Json::dump(int indent) const {
    std::ostringstream output;
    dumpValue(output, *this, indent, 0);
    if (indent >= 0) {
        output << '\n';
    }
    return output.str();
}

Result<Json> Json::parse(const std::string& text) {
    Parser parser(text);
    return parser.parse();
}

Result<Json> Json::parseFile(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::IoError;
        diagnostic.message = "cannot open JSON file: " + path;
        return Result<Json>::failure(std::move(diagnostic));
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (!input.good() && !input.eof()) {
        Diagnostic diagnostic;
        diagnostic.code = ErrorCode::IoError;
        diagnostic.message = "cannot read JSON file: " + path;
        return Result<Json>::failure(std::move(diagnostic));
    }
    auto parsed = parse(buffer.str());
    if (!parsed.ok()) {
        parsed.error.message = path + ": " + parsed.error.message;
    }
    return parsed;
}

bool jsonBool(const Json& object, const std::string& key, bool fallback, Diagnostic& error) {
    const Json* value = object.find(key);
    if (value == nullptr || value->isNull()) {
        return fallback;
    }
    if (!value->isBool()) {
        error.code = ErrorCode::InvalidArgument;
        error.message = "config field '" + key + "' must be a boolean";
        return fallback;
    }
    return value->asBool();
}

int jsonInt(const Json& object, const std::string& key, int fallback, Diagnostic& error) {
    const Json* value = object.find(key);
    if (value == nullptr || value->isNull()) {
        return fallback;
    }
    if (!value->isNumber()) {
        error.code = ErrorCode::InvalidArgument;
        error.message = "config field '" + key + "' must be a number";
        return fallback;
    }
    try {
        return value->asInt();
    } catch (const std::exception&) {
        error.code = ErrorCode::InvalidArgument;
        error.message = "config field '" + key + "' is out of range";
        return fallback;
    }
}

std::uint64_t jsonUInt64(const Json& object, const std::string& key, std::uint64_t fallback,
                         Diagnostic& error) {
    const Json* value = object.find(key);
    if (value == nullptr || value->isNull()) {
        return fallback;
    }
    if (!value->isNumber()) {
        error.code = ErrorCode::InvalidArgument;
        error.message = "config field '" + key + "' must be a number";
        return fallback;
    }
    try {
        return value->asUInt64();
    } catch (const std::exception&) {
        error.code = ErrorCode::InvalidArgument;
        error.message = "config field '" + key + "' is out of range";
        return fallback;
    }
}

double jsonDouble(const Json& object, const std::string& key, double fallback,
                  Diagnostic& error) {
    const Json* value = object.find(key);
    if (value == nullptr || value->isNull()) {
        return fallback;
    }
    if (!value->isNumber()) {
        error.code = ErrorCode::InvalidArgument;
        error.message = "config field '" + key + "' must be a number";
        return fallback;
    }
    return value->asNumber();
}

std::string jsonString(const Json& object, const std::string& key, const std::string& fallback,
                       Diagnostic& error) {
    const Json* value = object.find(key);
    if (value == nullptr || value->isNull()) {
        return fallback;
    }
    if (!value->isString()) {
        error.code = ErrorCode::InvalidArgument;
        error.message = "config field '" + key + "' must be a string";
        return fallback;
    }
    return value->asString();
}

}  // namespace font_atlas
