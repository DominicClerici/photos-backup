// PhotoFacts reads the parts of a PHAsset that expo-media-library's bridge does
// not expose.
//
// The library's Asset has ten getters. PHAsset has around forty properties, and
// what lies in the gap is not a nicety: the Hidden album, which frame of a burst
// a person kept, whether the shot came off this camera or an iTunes sync or
// somebody else's shared album, whether it has been edited at all, and the
// altitude and accuracy of the fix that CoreLocation recorded beside the two
// coordinates everyone remembers. None of it is in the file. All of it is gone
// the day the phone is wiped or the photo is deleted from it, and unlike a
// Google Takeout sitting on a disk there is no second pass to be had.
//
// So this captures rather than models. Apple's own enum values and constant
// names are carried through as they are, raw value beside readable label,
// because a mapping onto names of ours would be a guess while the values are the
// fact — the same rule internal/photokit applies on the server. Duplication with
// what expo already reports costs bytes; a gap costs the fact.
//
// Three things it deliberately does not do. It never asks for photo library
// authorization, because expo-media-library owns that prompt and a second one
// would be a mystery to the person holding the phone. It never touches
// PHImageManager or PHAssetResourceManager, because those can pull an original
// down from iCloud and this app runs against a phone with iCloud Photos off. And
// it never throws: it is called once per asset across a library of tens of
// thousands, always after the bytes are already archived, so an asset that
// cannot answer one question must still answer the rest.

import CoreLocation
import ExpoModulesCore
import Foundation
import Photos

public class PhotoFactsModule: Module {
  public func definition() -> ModuleDefinition {
    Name("PhotoFacts")

    // Resolves with null rather than rejecting when the identifier no longer
    // names anything. A deleted photo and a photo this build is not allowed to
    // see are the same answer here, and neither is an error the caller can act
    // on.
    AsyncFunction("factsForAssetAsync") { (localId: String, promise: Promise) in
      guard let asset = findAsset(localId) else {
        promise.resolve()
        return
      }
      promise.resolve(facts(of: asset))
    }
  }
}

/// The scheme expo-media-library puts in front of every identifier it hands to
/// JavaScript, and therefore what the upload queue has stored for every asset it
/// knows about.
private let assetUriPrefix = "ph://"

private func findAsset(_ localId: String) -> PHAsset? {
  let identifier = localId.hasPrefix(assetUriPrefix)
    ? String(localId.dropFirst(assetUriPrefix.count))
    : localId
  if identifier.isEmpty {
    return nil
  }

  // Both of these default to false, and both defaults would defeat the point of
  // this module. A fetch that excludes hidden assets cannot answer isHidden for
  // the one asset whose answer is interesting, and a fetch that excludes the
  // secondary frames of a burst cannot answer burstIdentifier for the frames
  // that make it a burst — it would resolve nothing and this would report the
  // asset as gone. expo-media-library sets both on its own by-identifier fetch,
  // in Asset.loadPHAsset(), for the same reason.
  let options = PHFetchOptions()
  options.includeHiddenAssets = true
  options.includeAllBurstAssets = true
  options.fetchLimit = 1

  return PHAsset.fetchAssets(withLocalIdentifiers: [identifier], options: options).firstObject
}

private func facts(of asset: PHAsset) -> [String: Any?] {
  // Local metadata only — the resource inventory is what PhotoKit already holds
  // about the asset, not a request for any of its bytes.
  let resources = PHAssetResource.assetResources(for: asset)
  let hasAdjustments = resources.contains { $0.type == .adjustmentData }
  let described = resources.map(describe(resource:))

  return [
    "localId": asset.localIdentifier,
    "hidden": asset.isHidden,
    "favorite": asset.isFavorite,
    "mediaType": mediaType(of: asset),
    "mediaSubtypes": mediaSubtypes(of: asset),
    "sourceType": sourceType(of: asset),
    "playbackStyle": playbackStyle(of: asset),
    "burstIdentifier": asset.burstIdentifier,
    "burstSelectionTypes": burstSelectionTypes(of: asset),
    "representsBurst": asset.representsBurst,
    "pixelWidth": asset.pixelWidth,
    "pixelHeight": asset.pixelHeight,
    "durationSeconds": asset.duration,
    "createdAt": iso8601(asset.creationDate),
    "modifiedAt": iso8601(asset.modificationDate),
    // The one fact here that is about the bytes rather than beside them: an
    // asset carrying adjustment data has an edited render, and the archive
    // needs to know that the thing it stored may be the render.
    "hasAdjustments": hasAdjustments,
    "originalFilename": originalFilename(from: resources),
    "resources": described,
    "location": location(of: asset)
  ]
}

/// The name the camera gave the file, which is not always the name
/// expo-media-library reports: an edited asset grows a second, rendered resource
/// and the library has no reason to prefer either one.
private func originalFilename(from resources: [PHAssetResource]) -> String? {
  let primary = resources.first { resource in
    resource.type == .photo || resource.type == .video || resource.type == .audio
  }
  return (primary ?? resources.first)?.originalFilename
}

private func describe(resource: PHAssetResource) -> [String: Any?] {
  return [
    "type": resourceType(of: resource),
    "originalFilename": resource.originalFilename,
    "uniformTypeIdentifier": resource.uniformTypeIdentifier
  ]
}

// MARK: - Apple's enumerations, carried rather than translated
//
// Each of these answers with the raw value beside a label. The label is Apple's
// own constant name, so a reader who wants to know what it meant has something
// to search for; the raw value is what survives a case this file has never heard
// of, which is the whole reason both are here.

private func mediaType(of asset: PHAsset) -> [String: Any] {
  let type = asset.mediaType
  let name: String
  switch type {
  case .unknown: name = "unknown"
  case .image: name = "image"
  case .video: name = "video"
  case .audio: name = "audio"
  @unknown default: name = "unrecognized"
  }
  return ["value": type.rawValue, "name": name]
}

private func playbackStyle(of asset: PHAsset) -> [String: Any] {
  let style = asset.playbackStyle
  let name: String
  switch style {
  case .unsupported: name = "unsupported"
  case .image: name = "image"
  case .imageAnimated: name = "imageAnimated"
  case .livePhoto: name = "livePhoto"
  case .video: name = "video"
  case .videoLooping: name = "videoLooping"
  @unknown default: name = "unrecognized"
  }
  return ["value": style.rawValue, "name": name]
}

private func resourceType(of resource: PHAssetResource) -> [String: Any] {
  let type = resource.type
  let name: String
  switch type {
  case .photo: name = "photo"
  case .video: name = "video"
  case .audio: name = "audio"
  case .alternatePhoto: name = "alternatePhoto"
  case .fullSizePhoto: name = "fullSizePhoto"
  case .fullSizeVideo: name = "fullSizeVideo"
  case .adjustmentData: name = "adjustmentData"
  case .adjustmentBasePhoto: name = "adjustmentBasePhoto"
  case .pairedVideo: name = "pairedVideo"
  case .fullSizePairedVideo: name = "fullSizePairedVideo"
  case .adjustmentBasePairedVideo: name = "adjustmentBasePairedVideo"
  case .adjustmentBaseVideo: name = "adjustmentBaseVideo"
  @unknown default: name = "unrecognized"
  }
  return ["value": type.rawValue, "name": name]
}

// The three below are option sets rather than enumerations, so they answer with
// every name that is set. The raw bitmask goes along for the same reason as
// above: a bit this file cannot name is still recorded.

private func mediaSubtypes(of asset: PHAsset) -> [String: Any] {
  let subtypes = asset.mediaSubtypes
  var names: [String] = []
  if subtypes.contains(.photoPanorama) { names.append("photoPanorama") }
  if subtypes.contains(.photoHDR) { names.append("photoHDR") }
  if subtypes.contains(.photoScreenshot) { names.append("photoScreenshot") }
  if subtypes.contains(.photoLive) { names.append("photoLive") }
  if subtypes.contains(.photoDepthEffect) { names.append("photoDepthEffect") }
  if subtypes.contains(.videoStreamed) { names.append("videoStreamed") }
  if subtypes.contains(.videoHighFrameRate) { names.append("videoHighFrameRate") }
  if subtypes.contains(.videoTimelapse) { names.append("videoTimelapse") }
  if subtypes.contains(.videoCinematic) { names.append("videoCinematic") }
  if subtypes.contains(.spatialMedia) { names.append("spatialMedia") }
  return ["value": Int(bitPattern: subtypes.rawValue), "names": names]
}

private func sourceType(of asset: PHAsset) -> [String: Any] {
  let source = asset.sourceType
  var names: [String] = []
  if source.contains(.typeUserLibrary) { names.append("typeUserLibrary") }
  if source.contains(.typeCloudShared) { names.append("typeCloudShared") }
  if source.contains(.typeiTunesSynced) { names.append("typeiTunesSynced") }
  return ["value": Int(bitPattern: source.rawValue), "names": names]
}

private func burstSelectionTypes(of asset: PHAsset) -> [String: Any] {
  let selection = asset.burstSelectionTypes
  var names: [String] = []
  if selection.contains(.autoPick) { names.append("autoPick") }
  if selection.contains(.userPick) { names.append("userPick") }
  return ["value": Int(bitPattern: selection.rawValue), "names": names]
}

// MARK: - The rest of the fix

/// Everything CoreLocation recorded, not just the two coordinates.
///
/// Course, speed and the two accuracies are passed through exactly as they
/// arrive, negatives and all: CoreLocation writes a negative there for "not
/// measured", and rewriting that as absence is a reading of the data rather than
/// the data, which is a decision better made by whatever asks the archive later.
private func location(of asset: PHAsset) -> [String: Any?]? {
  guard let fix = asset.location else {
    return nil
  }
  let coordinate = fix.coordinate
  return [
    "latitude": coordinate.latitude,
    "longitude": coordinate.longitude,
    "altitude": fix.altitude,
    "horizontalAccuracy": fix.horizontalAccuracy,
    "verticalAccuracy": fix.verticalAccuracy,
    "course": fix.course,
    "speed": fix.speed,
    "timestamp": iso8601(fix.timestamp)
  ]
}

private enum Timestamps {
  /// Built once. This runs per asset across a whole library, and a formatter is
  /// the expensive part of writing a date.
  static let iso8601: ISO8601DateFormatter = {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    return formatter
  }()
}

private func iso8601(_ date: Date?) -> String? {
  guard let date else {
    return nil
  }
  return Timestamps.iso8601.string(from: date)
}
