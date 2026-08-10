import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  AppState,
  Platform,
  SafeAreaView,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  View,
} from 'react-native';
import { StatusBar } from 'expo-status-bar';
import * as MediaLibrary from 'expo-media-library';

import { countLibrary } from './src/library';
import { clearLog, formatTime, loadPersisted, log as rawLog, subscribe, type LogLine } from './src/log';
import { fetchTimeline, loadServerUrl, ping, resetTimeline, saveServerUrl } from './src/net';
import { runMd5, runLegacyMd5 } from './src/tests/q1-md5';
import { runUploadSourceMatrix } from './src/tests/q1b-uploadsource';
import { cancelActive, runBackgroundUpload } from './src/tests/q2-background';
import { runLivePhoto } from './src/tests/q3-livephoto';
import { runMdns } from './src/tests/q4-mdns';
import type { TestContext, TestResult } from './src/tests/types';
import { errText } from './src/tests/types';
import { Btn, C, Card, KV, LivenessBar, Row, Verdict, mono } from './src/ui';

type Slot = { busy: boolean; result: TestResult | null; error: string | null };
const IDLE: Slot = { busy: false, result: null, error: null };

export default function App() {
  const [serverUrl, setServerUrl] = useState(loadServerUrl);
  const [health, setHealth] = useState<string>('not checked');
  const [healthOk, setHealthOk] = useState<boolean | null>(null);
  const [perm, setPerm] = useState<MediaLibrary.PermissionResponse | null>(null);
  const [library, setLibrary] = useState<string>('—');
  const [lines, setLines] = useState<LogLine[]>([]);
  const [slots, setSlots] = useState<Record<string, Slot>>({});
  const [sizeMb, setSizeMb] = useState('20');
  const [throttleKb, setThrottleKb] = useState('200');

  useEffect(() => subscribe(setLines), []);

  useEffect(() => {
    const restored = loadPersisted();
    rawLog('app', `launched (restored ${restored} lines from a previous session)`);
    const sub = AppState.addEventListener('change', (state) => rawLog('appstate', state));
    MediaLibrary.getPermissionsAsync().then(setPerm);
    return () => sub.remove();
  }, []);

  const slot = (key: string) => slots[key] ?? IDLE;

  const run = useCallback(
    async (key: string, fn: (ctx: TestContext) => Promise<TestResult>) => {
      setSlots((s) => ({ ...s, [key]: { busy: true, result: null, error: null } }));
      rawLog(key, '--- run start ---');
      const ctx: TestContext = { serverUrl, log: (m) => rawLog(key, m) };
      try {
        const result = await fn(ctx);
        rawLog(key, `--- ${result.ok ? 'PASS' : 'FAIL'}: ${result.verdict} ---`);
        setSlots((s) => ({ ...s, [key]: { busy: false, result, error: null } }));
      } catch (e) {
        rawLog(key, `--- THREW: ${errText(e)} ---`);
        setSlots((s) => ({ ...s, [key]: { busy: false, result: null, error: errText(e) } }));
      }
    },
    [serverUrl]
  );

  const checkHealth = useCallback(async () => {
    setHealth('checking…');
    try {
      const { ms, body } = await ping(serverUrl);
      setHealth(`${body.name} · ${ms} ms`);
      setHealthOk(true);
      rawLog('health', `${serverUrl} ok in ${ms}ms`);
    } catch (e) {
      setHealth(errText(e));
      setHealthOk(false);
      rawLog('health', `${serverUrl} failed: ${errText(e)}`);
    }
  }, [serverUrl]);

  const requestPerm = useCallback(async () => {
    const res = await MediaLibrary.requestPermissionsAsync();
    setPerm(res);
    rawLog('perm', `status=${res.status} accessPrivileges=${res.accessPrivileges ?? 'n/a'}`);
    if (res.granted) {
      const counts = await countLibrary();
      setLibrary(`${counts.photos} photos · ${counts.videos} videos`);
      rawLog('perm', `library: ${counts.photos} photos, ${counts.videos} videos`);
    }
  }, []);

  const limited = perm?.accessPrivileges === 'limited';
  const permTone = !perm?.granted ? 'bad' : limited ? 'warn' : 'ok';

  const bgOptions = useMemo(
    () => ({
      sizeMb: Math.max(1, Number(sizeMb) || 20),
      throttleBytesPerSec: Math.max(10_000, (Number(throttleKb) || 200) * 1024),
    }),
    [sizeMb, throttleKb]
  );

  return (
    <View style={st.root}>
      <StatusBar style="light" />
      <SafeAreaView style={st.safe}>
        <ScrollView contentContainerStyle={st.scroll} keyboardShouldPersistTaps="handled">
          <Text style={st.h1}>photos-backup · phase 0</Text>
          <Text style={st.sub}>
            Four Expo assumptions, answered on real hardware. Every result that matters is
            cross-checked against the test server rather than trusted from the device.
          </Text>

          <Card
            n="00"
            title="Environment"
            tone={healthOk === null ? 'idle' : healthOk ? 'ok' : 'bad'}
            status={healthOk === null ? 'unknown' : healthOk ? 'reachable' : 'unreachable'}>
            <Text style={st.label}>test server</Text>
            <TextInput
              style={st.input}
              value={serverUrl}
              onChangeText={(t) => {
                setServerUrl(t.trim());
                saveServerUrl(t.trim());
              }}
              autoCapitalize="none"
              autoCorrect={false}
              keyboardType="url"
              placeholderTextColor={C.dim}
            />
            <Row>
              <Btn label="Ping" onPress={checkHealth} />
              <Btn
                label="Reset timeline"
                tone="quiet"
                onPress={() => resetTimeline(serverUrl).catch(() => {})}
              />
            </Row>
            <KV k="health" v={health} />
            <KV k="platform" v={`${Platform.OS} ${Platform.Version}`} />

            <View style={st.divider} />

            <Row>
              <Btn label="Photo permission" onPress={requestPerm} />
              {limited ? (
                <Btn
                  label="Fix limited access"
                  tone="danger"
                  onPress={() => MediaLibrary.presentPermissionsPicker()}
                />
              ) : null}
            </Row>
            <KV k="status" v={perm ? `${perm.status} (granted=${perm.granted})` : 'not requested'} />
            <KV k="accessPrivileges" v={perm?.accessPrivileges ?? '—'} />
            <KV k="library" v={library} />
            {limited ? (
              <Verdict
                ok={false}
                text={'Risk #4 confirmed live: "Select Photos" silently hides the rest of the library. accessPrivileges is how v1 detects it.'}
              />
            ) : null}
            {perm?.granted && !limited ? (
              <Verdict ok text="Full library access — accessPrivileges === 'all'." />
            ) : null}
          </Card>

          <Card
            n="Q1"
            title="Native MD5 of a large file"
            question="Can expo-file-system hash a multi-hundred-MB video without loading it into JS or into RAM — and what does it cost?"
            tone={tone(slot('q1'))}
            status={status(slot('q1'))}>
            <LivenessBar />
            <Text style={st.hint}>
              Watch the bar while it hashes. File.md5 is a synchronous JSI property, so a freeze here
              is the real cost of the API.
            </Text>
            <Row>
              <Btn label="Run" busy={slot('q1').busy} onPress={() => run('q1', runMd5)} />
              <Btn
                label="Legacy (may crash)"
                tone="danger"
                busy={slot('q1-legacy').busy}
                onPress={() => run('q1-legacy', runLegacyMd5)}
              />
            </Row>
            <Outcome slot={slot('q1')} />
            <Outcome slot={slot('q1-legacy')} />
          </Card>

          <Card
            n="Q1b"
            title="Where can an upload read from?"
            question="Q1 uploaded a PhotoKit original and failed; Q3 uploaded from the app container and succeeded. Which variable actually matters — the sandbox, or the URI fragment?"
            tone={tone(slot('q1b'))}
            status={status(slot('q1b'))}>
            <Text style={st.hint}>
              Runs the smallest video five ways: PhotoKit uri and fragment-stripped uri, each on a
              background and a foreground session, plus a copy inside the app container.
            </Text>
            <Row>
              <Btn label="Run matrix" busy={slot('q1b').busy} onPress={() => run('q1b', runUploadSourceMatrix)} />
            </Row>
            <Outcome slot={slot('q1b')} />
          </Card>

          <Card
            n="Q2"
            title="Background upload survives suspension"
            question="Does a sessionType:'background' upload keep sending bytes after the app is suspended — and does the JS side ever find out?"
            tone={tone(slot('q2'))}
            status={status(slot('q2'))}>
            <Row>
              <View style={st.field}>
                <Text style={st.label}>payload MB</Text>
                <TextInput
                  style={[st.input, st.inputSm]}
                  value={sizeMb}
                  onChangeText={setSizeMb}
                  keyboardType="number-pad"
                />
              </View>
              <View style={st.field}>
                <Text style={st.label}>throttle KB/s</Text>
                <TextInput
                  style={[st.input, st.inputSm]}
                  value={throttleKb}
                  onChangeText={setThrottleKb}
                  keyboardType="number-pad"
                />
              </View>
              <View style={st.field}>
                <Text style={st.label}>expected</Text>
                <Text style={st.expected}>
                  ~{((bgOptions.sizeMb * 1024 * 1024) / bgOptions.throttleBytesPerSec).toFixed(0)}s
                </Text>
              </View>
            </Row>
            <Text style={st.hint}>
              Start it, then press Home and count. Byte arrival is recorded server-side, so the answer
              does not depend on the app being alive to report it.
            </Text>
            <Row>
              <Btn
                label="Run (background)"
                busy={slot('q2').busy}
                onPress={() => run('q2', (ctx) => runBackgroundUpload(ctx, { ...bgOptions, sessionType: 'background' }))}
              />
              <Btn
                label="Control (foreground)"
                tone="quiet"
                busy={slot('q2-fg').busy}
                onPress={() => run('q2-fg', (ctx) => runBackgroundUpload(ctx, { ...bgOptions, sessionType: 'foreground' }))}
              />
              <Btn label="Cancel" tone="danger" onPress={cancelActive} />
            </Row>
            <Outcome slot={slot('q2')} />
            <Outcome slot={slot('q2-fg')} />
            <ServerTimeline serverUrl={serverUrl} />
          </Card>

          <Card
            n="Q3"
            title="Live Photo paired .mov"
            question="Can the companion video of a Live Photo be retrieved through expo-media-library, or does this force a Swift module?"
            tone={tone(slot('q3'))}
            status={status(slot('q3'))}>
            <Row>
              <Btn label="Run" busy={slot('q3').busy} onPress={() => run('q3', runLivePhoto)} />
            </Row>
            <Outcome slot={slot('q3')} />
          </Card>

          <Card
            n="Q4"
            title="mDNS discovery from a dev client"
            question="Does react-native-zeroconf resolve _photobackup._tcp under RN 0.86 bridgeless, and is the address it returns dialable?"
            tone={tone(slot('q4'))}
            status={status(slot('q4'))}>
            <Text style={st.hint}>
              iOS shows the Local Network prompt on the first scan. Accept it; if you miss it, the scan
              silently returns nothing.
            </Text>
            <Row>
              <Btn label="Scan 12s" busy={slot('q4').busy} onPress={() => run('q4', runMdns)} />
            </Row>
            <Outcome slot={slot('q4')} />
          </Card>

          <Card n="LOG" title="Device log" tone="idle" status={`${lines.length} lines`}>
            <Text style={st.hint}>
              Persisted to disk, so it survives a force-quit. Reloaded on launch.
            </Text>
            <Row>
              <Btn label="Clear" tone="quiet" onPress={clearLog} />
            </Row>
            <View style={st.logBox}>
              {lines.slice(-160).map((l) => (
                <Text key={l.seq} style={st.logLine} selectable>
                  <Text style={st.logTime}>{formatTime(l.t)} </Text>
                  <Text style={st.logTag}>{l.tag.padEnd(9)}</Text>
                  {l.msg}
                </Text>
              ))}
              {lines.length === 0 ? <Text style={st.logLine}>empty</Text> : null}
            </View>
          </Card>

          <View style={{ height: 40 }} />
        </ScrollView>
      </SafeAreaView>
    </View>
  );
}

function tone(s: Slot): 'ok' | 'bad' | 'warn' | 'idle' {
  if (s.busy) return 'warn';
  if (s.error) return 'bad';
  if (!s.result) return 'idle';
  return s.result.ok ? 'ok' : 'bad';
}

function status(s: Slot) {
  if (s.busy) return 'running';
  if (s.error) return 'threw';
  if (!s.result) return 'not run';
  return s.result.ok ? 'pass' : 'fail';
}

function Outcome({ slot }: { slot: Slot }) {
  if (slot.error) return <Verdict ok={false} text={slot.error} />;
  if (!slot.result) return null;
  return (
    <View style={{ gap: 8 }}>
      <Verdict ok={slot.result.ok} text={slot.result.verdict} />
      <View style={{ gap: 4 }}>
        {slot.result.details.map(([k, v]) => (
          <KV key={k} k={k} v={v} />
        ))}
      </View>
    </View>
  );
}

function ServerTimeline({ serverUrl }: { serverUrl: string }) {
  const [rows, setRows] = useState<string[]>([]);
  const load = useCallback(async () => {
    try {
      const events = await fetchTimeline(serverUrl);
      setRows(
        events.slice(-40).map((e) => {
          const d = Object.entries(e.detail)
            .filter(([k]) => k !== 'clientTime')
            .map(([k, v]) => `${k}=${v}`)
            .join(' ');
          return `${(e.ms / 1000).toFixed(1)}s ${e.event} ${d}`;
        })
      );
    } catch (e) {
      setRows([errText(e)]);
    }
  }, [serverUrl]);

  return (
    <View style={{ gap: 8 }}>
      <Row>
        <Btn label="Pull server timeline" tone="quiet" onPress={load} />
      </Row>
      {rows.length ? (
        <View style={st.logBox}>
          {rows.map((r, i) => (
            <Text key={i} style={st.logLine} selectable>
              {r}
            </Text>
          ))}
        </View>
      ) : null}
    </View>
  );
}

const st = StyleSheet.create({
  root: { flex: 1, backgroundColor: C.bg },
  safe: { flex: 1 },
  scroll: { padding: 14, paddingTop: 8 },
  h1: { color: C.text, fontSize: 22, fontWeight: '700', letterSpacing: -0.3 },
  sub: { color: C.dim, fontSize: 12.5, lineHeight: 18, marginTop: 6, marginBottom: 18 },
  label: { color: C.dim, fontSize: 10, textTransform: 'uppercase', letterSpacing: 0.7 },
  hint: { color: C.dim, fontSize: 11.5, lineHeight: 16 },
  input: {
    backgroundColor: C.bg,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: C.border,
    borderRadius: 8,
    paddingHorizontal: 10,
    paddingVertical: 9,
    color: C.text,
    fontFamily: mono,
    fontSize: 12,
  },
  inputSm: { width: 96 },
  field: { gap: 4 },
  expected: { color: C.text, fontFamily: mono, fontSize: 12, paddingVertical: 9 },
  divider: {
    height: StyleSheet.hairlineWidth,
    backgroundColor: C.border,
    marginVertical: 4,
  },
  logBox: {
    backgroundColor: C.bg,
    borderRadius: 8,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: C.border,
    padding: 8,
    gap: 2,
  },
  logLine: { color: C.text, fontFamily: mono, fontSize: 9.5, lineHeight: 13 },
  logTime: { color: C.dim },
  logTag: { color: C.accent },
});
