#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace sl {

void writePngRGBA(const std::string& path,
                  int width,
                  int height,
                  const std::vector<std::uint8_t>& rgba);

} // namespace sl

