#pragma once

#include <cstdint>
#include <vector>

#include "shadowpipe/cascades.hpp"
#include "shadowpipe/scene.hpp"

namespace shadowpipe {

void rasterizeDepth(const std::vector<WorldTriangle>& triangles,
                    const LightFrame& frame, std::uint32_t width,
                    std::uint32_t height, std::vector<float>& depth_buffer,
                    const std::vector<std::uint8_t>* allowed_nodes = nullptr,
                    std::vector<std::uint32_t>* owner_buffer = nullptr);

}  // namespace shadowpipe

