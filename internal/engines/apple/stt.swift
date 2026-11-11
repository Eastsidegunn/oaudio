// oaudio-apple-stt: on-device speech recognition through Apple's SpeechAnalyzer
// (macOS 26+). It is a helper for the "apple" runner, not a runner itself.
//
// Protocol: one JSON request per line on stdin, JSON lines on stdout.
//   {"op":"transcribe","path":"/tmp/a.wav","locale":"en-US"}
//     -> {"segment":{"start":0.0,"end":2.5,"text":"...","words":[{"word":"..","start":..,"end":..}]}} ...
//     -> {"done":true}
//   {"op":"locales"} -> {"locales":["en-US", ...]}
// Any failure answers {"error":"..."} and the helper keeps running.
//
// Audio never leaves the machine: SpeechTranscriber runs on device.

import AVFoundation
import Foundation
import Speech

func emit(_ obj: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: obj),
          let line = String(data: data, encoding: .utf8) else { return }
    print(line)
    fflush(stdout)
}

@available(macOS 26.0, *)
func transcribe(path: String, localeID: String) async throws {
    let wanted = Locale(identifier: localeID)
    guard let locale = await SpeechTranscriber.supportedLocale(equivalentTo: wanted) else {
        throw NSError(domain: "oaudio", code: 1, userInfo: [
            NSLocalizedDescriptionKey: "locale \(localeID) is not supported by on-device recognition",
        ])
    }
    let transcriber = SpeechTranscriber(
        locale: locale, transcriptionOptions: [], reportingOptions: [],
        attributeOptions: [.audioTimeRange])
    // The first use of a locale downloads Apple's on-device model for it.
    if await AssetInventory.status(forModules: [transcriber]) != .installed,
       let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
        FileHandle.standardError.write("downloading the on-device model for \(locale.identifier)\n".data(using: .utf8)!)
        try await request.downloadAndInstall()
    }
    let analyzer = SpeechAnalyzer(modules: [transcriber])
    let file = try AVAudioFile(forReading: URL(fileURLWithPath: path))

    async let consume: Void = {
        for try await result in transcriber.results where result.isFinal {
            var words: [[String: Any]] = []
            var start: Double? = nil
            var end = 0.0
            for run in result.text.runs {
                guard let range = run.audioTimeRange else { continue }
                let text = String(result.text[run.range].characters)
                if start == nil { start = range.start.seconds }
                end = range.end.seconds
                let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
                if !trimmed.isEmpty {
                    words.append(["word": trimmed, "start": range.start.seconds, "end": range.end.seconds])
                }
            }
            let text = String(result.text.characters).trimmingCharacters(in: .whitespacesAndNewlines)
            if text.isEmpty { continue }
            emit(["segment": ["start": start ?? 0, "end": end, "text": text, "words": words]])
        }
    }()

    if let last = try await analyzer.analyzeSequence(from: file) {
        try await analyzer.finalizeAndFinish(through: last)
    } else {
        await analyzer.cancelAndFinishNow()
    }
    try await consume
}

guard #available(macOS 26.0, *) else {
    emit(["error": "on-device recognition needs macOS 26 or later"])
    exit(2)
}

while let line = readLine(strippingNewline: true) {
    guard let data = line.data(using: .utf8),
          let req = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
          let op = req["op"] as? String else {
        emit(["error": "malformed request"])
        continue
    }
    switch op {
    case "locales":
        let locales = await SpeechTranscriber.supportedLocales
        emit(["locales": locales.map { $0.identifier(.bcp47) }.sorted()])
    case "transcribe":
        guard let path = req["path"] as? String else {
            emit(["error": "path is required"])
            continue
        }
        do {
            try await transcribe(path: path, localeID: (req["locale"] as? String) ?? "en-US")
            emit(["done": true])
        } catch {
            emit(["error": error.localizedDescription])
        }
    default:
        emit(["error": "unknown op \(op)"])
    }
}
