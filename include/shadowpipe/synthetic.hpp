#pragma once

#include <string>

#include "shadowpipe/error.hpp"
#include "shadowpipe/scene.hpp"

namespace shadowpipe {

enum class SyntheticVariant {
    Valid,
    BadIndex,
    BadTransform,
    BadBounds,
};

Scene makeSyntheticScene();
Status writeSyntheticScene(const std::string& path, SyntheticVariant variant);
Status generateAllSyntheticSamples(const std::string& directory);

}  // namespace shadowpipe

