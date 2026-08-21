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
// Two things it deliberately does not do. It never asks for photo library
// authorization, because expo-media-library owns that prompt and a second one
// would be a mystery to the person holding the phone. And it never touches
// PHImageManager or PHAssetResourceManager for bytes on behalf of a *library*
// asset, because those can pull an original down from iCloud and this app runs
// against a phone with iCloud Photos off — the resource inventory below is
// metadata PhotoKit already holds, not a request for any of it.
//
// fetchSharedResourceAsync is the one exception, and it is narrow on purpose: an
// iCloud Shared Album asset has no local original for the rule to protect. The
// phone holds a cached rendition of somebody else's upload and nothing else, so
// a fetch is the only way to read one at all, and the function is unreachable
// for anything that is not in a shared album. The rule above is unchanged for
// every asset it was written about.
//
// The module also carries two things the sync engine cannot get cheaply from
// expo-media-library, and they are here rather than in a module of their own
// because they are the same complaint about the same bridge: listing a library
// costs one crossing per asset when it should cost one, and hashing a file
// happens on the thread that is trying to run the app.
//
//   factsForAssetAsync  never throws. It is called once per asset across a
//                       library of tens of thousands, always after the bytes are
//                       already archived, so an asset that cannot answer one
//                       question must still answer the rest.
//   enumerateAsync      never throws, for the same reason it exists: it is the
//                       first step of every run and an empty answer is a run
//                       that does nothing, which the caller can see.
//   md5ForFileAsync     does throw, and must. A digest is a claim about bytes
//                       the server will verify, so "could not read it" has to
//                       reach the queue as this item's failure rather than as a
//                       hash of nothing.
//
// And two more for the shared-album survey, which is scaffolding rather than
// backup: nothing in the sync engine calls either of these yet.
//
//   sharedAlbumsAsync         never throws, like the enumerator it sits beside.
//                             A phone with Shared Albums switched off has none,
//                             which is an empty list rather than a failure.
//   fetchSharedResourceAsync  never throws, and reports its failures as data
//                             instead. It is the odd one out and the reason is
//                             the caller: a run of these is driven by a loop
//                             that retries, paces itself and decides when iCloud
//                             has had enough, and that loop has to be able to
//                             tell a timeout from a withdrawn album. An
//                             exception carries whatever the bridge chose to put
//                             on it; a resolved dictionary carries the NSError
//                             domain and code exactly as Apple wrote them, plus
//                             how many bytes had arrived before it gave up.
//                             Zero bytes is never mistaken for success, because
//                             `ok` says which of the two it is.
//
// It also emits the module's one event, onSharedFetchProgress, while a fetch is
// in flight. A shared video takes seconds to come down from iCloud and a run of
// them takes minutes, and a screen showing nothing during that is
// indistinguishable from a screen showing a hang.

import CoreLocation
import CryptoKit
import ExpoModulesCore
import Foundation
import Photos

/// Hashing runs on a queue of its own.
///
/// Every AsyncFunction that does not name a queue shares one serial
/// DispatchQueue with every other Expo module in the app. Hashing a 1GB video
/// there would hold up the calls the upload path is making at the same time —
/// which is exactly the serialization this was moved off the JS thread to end.
private let hashQueue = DispatchQueue(label: "photofacts.hash", qos: .utility)

/// Read size for the hash. Larger than the 64KB expo-file-system uses: this runs
/// off the JS thread, so the only thing the block size buys or costs is syscalls
/// against peak memory, and a 1GB video is worth the megabyte.
private let hashBlockSize = 1024 * 1024

/// The module's one event: how far the shared-album fetch in flight has got.
private let sharedFetchProgressEvent = "onSharedFetchProgress"

/// How often a fetch in flight is allowed to report itself, in seconds.
///
/// PhotoKit calls both of its handlers far more often than a screen can be read
/// — a video arrives in thousands of chunks — and every report is a crossing of
/// the bridge and a React render. Six a second is past what the eye resolves and
/// a small fraction of what PhotoKit offers.
private let progressReportInterval = 0.15

/// The domain on a failure this module found itself, as opposed to one Apple
/// reported. Callers classify on domain and code rather than on the message, so
/// ours has to be as distinguishable as Apple's is.
private let photoFactsErrorDomain = "PhotoFacts"

/// No photo, video or audio resource on the asset at all. Nothing to fetch, and
/// nothing a retry would change.
private let noResourceCode = 404

public class PhotoFactsModule: Module {
  public func definition() -> ModuleDefinition {
    Name("PhotoFacts")

    Events(sharedFetchProgressEvent)

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

    // The whole library in one call. See listAssets for what this replaces.
    AsyncFunction("enumerateAsync") { (limit: Int, promise: Promise) in
      promise.resolve(listAssets(limit: limit))
    }

    // Every iCloud Shared Album, with what is in it. See sharedAlbums().
    AsyncFunction("sharedAlbumsAsync") { (promise: Promise) in
      promise.resolve(sharedAlbums())
    }

    // The one call in this module that goes to the network, and the only one
    // allowed to. See fetchResource().
    //
    // Weakly, because the closure outlives the call and the module owns the
    // definition that owns the closure. A fetch still in flight when the module
    // goes away simply stops reporting itself, which is the correct amount of
    // fuss to make about progress on a screen that is no longer there.
    AsyncFunction("fetchSharedResourceAsync") { [weak self] (localId: String, promise: Promise) in
      guard let asset = findAsset(localId) else {
        promise.resolve()
        return
      }
      fetchResource(of: asset, localId: localId, promise: promise) { body in
        self?.sendEvent(sharedFetchProgressEvent, body)
      }
    }

    AsyncFunction("md5ForFileAsync") { (uri: String, promise: Promise) in
      do {
        let digest = try md5Digest(ofFileAt: uri)
        promise.resolve(digest)
      } catch {
        promise.reject(error)
      }
    }
    .runOnQueue(hashQueue)
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

// MARK: - Enumeration

/// Every image and video in the library, newest capture first, in one call.
///
/// What this replaces: a Query().exeForMetadata() for the list, and then — because
/// the metadata expo-media-library returns carries no subtypes — one further
/// `getMediaSubtypes()` per image to ask the single question "is this a Live
/// Photo?". Each of those is its own PHAsset fetch and its own crossing of the
/// bridge, awaited in turn, on a library of tens of thousands, at the start of
/// every run including the one that has nothing to do. Here it is one fetch and
/// one crossing, and the subtype is already in hand.
///
/// Everything about the answer is chosen to match what expo-media-library
/// produced, because the identifiers are primary keys in a queue that already has
/// rows in it and the timestamps are what `sync/check` matches an archived asset
/// on. A `ph://` prefix on the identifier, milliseconds rounded rather than
/// truncated, `value(forKey: "filename")` for the name: all of it is
/// AssetMapper.toMetadata's behaviour, deliberately.
///
/// Left at PHFetchOptions' defaults for the same reason: hidden assets and the
/// non-representative frames of a burst stay out, as they always have. Including
/// them is a decision about what this app archives, not about how fast it can
/// list a library, and it does not belong in this change.
private func listAssets(limit: Int) -> [[String: Any?]] {
  let options = PHFetchOptions()
  options.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: false)]
  options.predicate = NSPredicate(
    format: "mediaType == %d || mediaType == %d",
    PHAssetMediaType.image.rawValue,
    PHAssetMediaType.video.rawValue
  )
  // A fetchLimit of 0 means "no limit" to PhotoKit and "the whole library" to
  // the caller, which is the same thing, but it is set only when asked so the
  // two never have to agree about it.
  if limit > 0 {
    options.fetchLimit = limit
  }

  let result = PHAsset.fetchAssets(with: options)
  var assets: [[String: Any?]] = []
  assets.reserveCapacity(result.count)

  result.enumerateObjects { asset, _, _ in
    let isImage = asset.mediaType == .image
    assets.append([
      "localId": assetUriPrefix + asset.localIdentifier,
      "kind": isImage ? "still" : "video",
      "filename": asset.value(forKey: "filename") as? String,
      "createdAt": milliseconds(asset.creationDate),
      "modifiedAt": milliseconds(asset.modificationDate),
      // The reason the whole function exists. Only an image can carry one, and
      // the paired video is a second queue entry the caller builds from this.
      "isLive": isImage && asset.mediaSubtypes.contains(.photoLive)
    ])
  }
  return assets
}

/// Milliseconds the way expo-media-library writes them — rounded, not truncated.
/// A queue full of rows timed one way cannot be matched against a server holding
/// them timed the other.
private func milliseconds(_ date: Date?) -> Int? {
  guard let date else {
    return nil
  }
  return Int((date.timeIntervalSince1970 * 1000.0).rounded())
}

// MARK: - Shared albums

/// Every iCloud Shared Album on the phone, with the assets in each.
///
/// A second enumeration rather than a wider one, because a shared album's assets
/// are not in the library at all. `PHFetchOptions.includeAssetSourceTypes`
/// defaults to `typeUserLibrary`, so listAssets() above cannot see them however
/// it is sorted or predicated: they exist only inside collections of subtype
/// `albumCloudShared`, and the way to them is through the collection.
///
/// Nesting the assets inside their album rather than returning one flat list is
/// the point of going through the collection: the album title is the only
/// provenance a shared asset carries, and an asset can be in several. Flattening
/// here would throw that away and then need a second pass to win it back.
///
/// Nothing is deduplicated. That is the caller's, because the caller is the one
/// that knows whether it wants a count of assets or a count of memberships.
private func sharedAlbums() -> [[String: Any?]] {
  let collections = PHAssetCollection.fetchAssetCollections(
    with: .album,
    subtype: .albumCloudShared,
    options: nil
  )

  var albums: [[String: Any?]] = []
  albums.reserveCapacity(collections.count)

  collections.enumerateObjects { collection, _, _ in
    let options = PHFetchOptions()
    options.sortDescriptors = [NSSortDescriptor(key: "creationDate", ascending: false)]

    let result = PHAsset.fetchAssets(in: collection, options: options)
    var assets: [[String: Any?]] = []
    assets.reserveCapacity(result.count)
    result.enumerateObjects { asset, _, _ in
      assets.append(describeShared(asset))
    }

    albums.append([
      "localId": collection.localIdentifier,
      "title": collection.localizedTitle,
      "startDate": milliseconds(collection.startDate),
      "endDate": milliseconds(collection.endDate),
      "assets": assets
    ])
  }
  return albums
}

/// One shared asset: what listAssets() reports, plus the things the survey
/// exists to find out.
///
/// The dimensions and the duration are here because Apple re-encodes everything
/// that goes into a Shared Album, and PHAsset already knows what came out — so
/// the question of what resolution survives the sharing can be answered across a
/// whole album without fetching a single byte.
///
/// The resource inventory answers the follow-up. A lone `photo` means the
/// downscaled render is all there is; a `fullSizePhoto` beside it would mean the
/// original is reachable after all, which would change the answer entirely.
private func describeShared(_ asset: PHAsset) -> [String: Any?] {
  let isImage = asset.mediaType == .image
  let resources = PHAssetResource.assetResources(for: asset)

  return [
    "localId": assetUriPrefix + asset.localIdentifier,
    "kind": isImage ? "still" : "video",
    "filename": asset.value(forKey: "filename") as? String,
    "createdAt": milliseconds(asset.creationDate),
    "modifiedAt": milliseconds(asset.modificationDate),
    "isLive": isImage && asset.mediaSubtypes.contains(.photoLive),
    "pixelWidth": asset.pixelWidth,
    "pixelHeight": asset.pixelHeight,
    "durationSeconds": asset.duration,
    // Expected to be exactly ["typeCloudShared"] for everything in here.
    // Carried anyway, because "expected" is what a survey is for.
    "sourceTypes": sourceType(of: asset),
    "resourceTypes": resources.map { resourceTypeName($0.type) }
  ]
}

/// The running totals for one fetch, and the decision about when to report them.
///
/// A lock, where the byte handler on its own would have needed none: PhotoKit
/// delivers data on one queue and download progress on another, and the two
/// touch the same counters. Each mutation hands back the body to send, or nil
/// when the last report was too recent to follow.
private final class FetchProgress {
  private let lock = NSLock()
  private let localId: String
  private var bytes = 0
  private var fraction = 0.0
  private var reportedAt = 0.0

  init(localId: String) {
    self.localId = localId
  }

  func received(_ count: Int) -> [String: Any?]? {
    return update { self.bytes += count }
  }

  func downloaded(_ value: Double) -> [String: Any?]? {
    return update { self.fraction = value }
  }

  func count() -> Int {
    lock.lock()
    defer { lock.unlock() }
    return bytes
  }

  private func update(_ change: () -> Void) -> [String: Any?]? {
    lock.lock()
    defer { lock.unlock() }
    change()

    let now = CFAbsoluteTimeGetCurrent()
    guard now - reportedAt >= progressReportInterval else { return nil }
    reportedAt = now
    return ["localId": localId, "bytes": bytes, "fraction": fraction]
  }
}

/// Reads one shared asset's primary resource all the way through, and reports
/// how big it turned out to be, how long it took, and how it failed if it did.
///
/// This is the one function in the module that goes to the network, and the
/// header's rule against it stands everywhere else. A library asset has bytes on
/// the disk and must never be fetched from iCloud. A shared asset has no local
/// original to fetch instead — the phone holds a cached rendition and nothing
/// more — so there is no reading one without asking Apple for it, and what comes
/// back is the thing the survey is trying to learn.
///
/// It counts the bytes rather than keeping them. A survey wants the size, the
/// elapsed time and the failure modes; writing whole albums' worth of downloads
/// to disk to learn that would be a backup, which is the thing this is meant to
/// inform rather than perform.
///
/// It resolves on failure instead of rejecting, which is the opposite of what
/// md5ForFileAsync does and for a reason that only applies here: the caller runs
/// these in a loop that retries, paces itself, and stops when iCloud has clearly
/// had enough. That loop decides on the domain and the code, and neither
/// survives being turned into an exception intact.
private func fetchResource(
  of asset: PHAsset,
  localId: String,
  promise: Promise,
  onProgress: @escaping ([String: Any?]) -> Void
) {
  let resources = PHAssetResource.assetResources(for: asset)
  guard let resource = primaryResource(from: resources) else {
    promise.resolve(fetchFailure(
      domain: photoFactsErrorDomain,
      code: noResourceCode,
      message: "the asset carries no photo, video or audio resource",
      bytes: 0,
      elapsedMs: 0
    ))
    return
  }

  let progress = FetchProgress(localId: localId)

  let options = PHAssetResourceRequestOptions()
  options.isNetworkAccessAllowed = true
  // The fraction and the byte count are not the same measurement and both are
  // worth having. This one is how much of the download iCloud has done, and it
  // is the only one that knows the total; the other is how much has actually
  // been handed over, and it is the only one that moves for an asset already
  // cached on the phone, where this never fires at all.
  options.progressHandler = { (fraction: Double) in
    if let body = progress.downloaded(fraction) { onProgress(body) }
  }

  let started = CFAbsoluteTimeGetCurrent()

  PHAssetResourceManager.default().requestData(
    for: resource,
    options: options,
    dataReceivedHandler: { data in
      if let body = progress.received(data.count) { onProgress(body) }
    },
    completionHandler: { error in
      let elapsedMs = Int(((CFAbsoluteTimeGetCurrent() - started) * 1000.0).rounded())
      let received = progress.count()

      if let error {
        // Apple's domain and code verbatim. Which of them mean "you are asking
        // too often" is exactly what nobody here knows yet, so the caller is
        // given the pair rather than a judgement about it.
        let ns = error as NSError
        promise.resolve(fetchFailure(
          domain: ns.domain,
          code: ns.code,
          message: ns.localizedDescription,
          bytes: received,
          elapsedMs: elapsedMs
        ))
        return
      }

      // Typed rather than inferred, because two of these are optional strings
      // and a literal left to itself infers [String: Any] — which boxes the
      // absent ones as optionals inside Any and bridges them as something no
      // caller wants. Every other dictionary in this file is coerced by its
      // function's return type; this one has nothing to be coerced by.
      let read: [String: Any?] = [
        "ok": true,
        "bytes": received,
        "elapsedMs": elapsedMs,
        "uniformTypeIdentifier": resource.uniformTypeIdentifier,
        "originalFilename": resource.originalFilename,
        "resourceType": resourceTypeName(resource.type)
      ]
      promise.resolve(read)
    }
  )
}

/// A failed read, in the shape the successful one resolves in. `ok` is what
/// tells them apart, so that zero bytes is never read as a fetch that worked.
private func fetchFailure(
  domain: String,
  code: Int,
  message: String,
  bytes: Int,
  elapsedMs: Int
) -> [String: Any?] {
  return [
    "ok": false,
    "domain": domain,
    "code": code,
    "message": message,
    "bytes": bytes,
    "elapsedMs": elapsedMs
  ]
}

/// The resource carrying the asset itself, as opposed to a thumbnail, an
/// adjustment, or a Live Photo's paired clip.
///
/// A full-size variant is preferred where one exists, because its presence is
/// the interesting case: it would mean the shared copy is not the downscale
/// everything else here assumes it is.
private func primaryResource(from resources: [PHAssetResource]) -> PHAssetResource? {
  if let full = resources.first(where: { $0.type == .fullSizePhoto || $0.type == .fullSizeVideo }) {
    return full
  }
  return resources.first { $0.type == .photo || $0.type == .video || $0.type == .audio }
}

// MARK: - Hashing

/// The MD5 of a file, computed off the JavaScript thread.
///
/// expo-file-system can already do this, and its result is identical — the same
/// CryptoKit MD5 over the same bytes. What it cannot do is get out of the way:
/// `File.md5` is a synchronous JSI property, so the hash runs on the JS thread
/// and nothing else in the app moves until it finishes. On a 1GB video that is
/// seconds of frozen interface and, worse, seconds in which the upload loop
/// cannot start the next transfer. Same digest, different thread.
private func md5Digest(ofFileAt uri: String) throws -> String {
  let url = fileURL(from: uri)

  // PhotoKit hands back URLs the app is extended access to. Claiming it is
  // harmless when there is nothing to claim, and the alternative — a read that
  // fails for a reason nothing here would explain — is not.
  let scoped = url.startAccessingSecurityScopedResource()
  defer {
    if scoped {
      url.stopAccessingSecurityScopedResource()
    }
  }

  let handle = try FileHandle(forReadingFrom: url)
  defer { try? handle.close() }

  var hasher = Insecure.MD5()
  while let block = try handle.read(upToCount: hashBlockSize), !block.isEmpty {
    hasher.update(data: block)
  }
  return hasher.finalize().map { String(format: "%02hhx", $0) }.joined()
}

/// Accepts what expo-file-system's `File.uri` hands out, and a bare path too.
private func fileURL(from uri: String) -> URL {
  guard let url = URL(string: uri), url.isFileURL else {
    return URL(fileURLWithPath: uri)
  }
  return url
}

// MARK: - Facts

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
  return ["value": resource.type.rawValue, "name": resourceTypeName(resource.type)]
}

/// The name on its own, for the shared-album survey — which reports an
/// inventory of types per asset and has no use for twelve copies of the raw
/// value beside them.
private func resourceTypeName(_ type: PHAssetResourceType) -> String {
  switch type {
  case .photo: return "photo"
  case .video: return "video"
  case .audio: return "audio"
  case .alternatePhoto: return "alternatePhoto"
  case .fullSizePhoto: return "fullSizePhoto"
  case .fullSizeVideo: return "fullSizeVideo"
  case .adjustmentData: return "adjustmentData"
  case .adjustmentBasePhoto: return "adjustmentBasePhoto"
  case .pairedVideo: return "pairedVideo"
  case .fullSizePairedVideo: return "fullSizePairedVideo"
  case .adjustmentBasePairedVideo: return "adjustmentBasePairedVideo"
  case .adjustmentBaseVideo: return "adjustmentBaseVideo"
  @unknown default: return "unrecognized"
  }
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
