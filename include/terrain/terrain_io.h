#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "terrain/status.h"
#include "terrain/types.h"

namespace terrain {

constexpr std::uint32_t kHeightHeaderSize = 92;
constexpr std::uint32_t kMaterialHeaderSize = 80;
constexpr std::uint32_t kPatchHeaderSize = 96;

Status loadHeightField(const std::string& path, HeightField* out);
Status parseHeightField(const std::vector<std::uint8_t>& bytes,
                        const std::string& source, HeightField* out);
Status saveHeightField(const std::string& path, const HeightField& field);
std::vector<std::uint8_t> encodeHeightField(const HeightField& field);

Status loadMaterialImage(const std::string& path, MaterialImage* out);
Status parseMaterialImage(const std::vector<std::uint8_t>& bytes,
                          const std::string& source, MaterialImage* out);
Status saveMaterialImage(const std::string& path,
                         const MaterialImage& material);
std::vector<std::uint8_t> encodeMaterialImage(const MaterialImage& material);

Status loadHeightPatch(const std::string& path, HeightPatch* out);
Status parseHeightPatch(const std::vector<std::uint8_t>& bytes,
                        const std::string& source, HeightPatch* out);
Status saveHeightPatch(const std::string& path, const HeightPatch& patch);
std::vector<std::uint8_t> encodeHeightPatch(const HeightPatch& patch);

Status loadMaterialPatch(const std::string& path, MaterialPatch* out);
Status parseMaterialPatch(const std::vector<std::uint8_t>& bytes,
                          const std::string& source, MaterialPatch* out);
Status saveMaterialPatch(const std::string& path,
                         const MaterialPatch& patch);
std::vector<std::uint8_t> encodeMaterialPatch(const MaterialPatch& patch);

Status validateMaterialDimensions(const HeightField& height,
                                  const MaterialImage& material);
Status validateBatch(const PatchBatch& batch, const HeightField& current,
                     const MaterialImage& material);

std::string computeHeightHash(const HeightField& field);
std::string computeMaterialHash(const MaterialImage& material);
std::string computeBatchHash(const PatchBatch& batch);

}  // namespace terrain

