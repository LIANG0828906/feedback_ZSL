function call(name, ...args) {
  return window.go.main.App[name](...args);
}

export function Initialize() { return call("Initialize"); }
export function GetState() { return call("GetState"); }
export function EditHeightDelta(blockID, x, y, delta) { return call("EditHeightDelta", blockID, x, y, delta); }
export function SetHeight(blockID, x, y, value) { return call("SetHeight", blockID, x, y, value); }
export function EditMaterial(layerID, biasX, biasY, biasZ, strength) {
  return call("EditMaterial", layerID, biasX, biasY, biasZ, strength);
}
export function SetDifferenceOperator(operator) { return call("SetDifferenceOperator", operator); }
export function SetScale(scale) { return call("SetScale", scale); }
export function RetryFailedBlocks() { return call("RetryFailedBlocks"); }
export function RetryBlock(blockID) { return call("RetryBlock", blockID); }
export function RebuildAll() { return call("RebuildAll"); }
export function ExportNormals() { return call("ExportNormals"); }
export function Describe() { return call("Describe"); }
