import { useEffect, useRef, useState, type ReactNode } from 'react';
import {
  ActivityIndicator,
  Animated,
  Easing,
  Pressable,
  StyleSheet,
  Text,
  View,
} from 'react-native';

export const C = {
  bg: '#0d1117',
  panel: '#161b22',
  panelAlt: '#1c2330',
  border: '#2b3441',
  text: '#e6edf3',
  dim: '#8b949e',
  accent: '#4493f8',
  ok: '#3fb950',
  bad: '#f85149',
  warn: '#d29922',
};

export const mono = 'Menlo';

export function Pill({ tone, children }: { tone: 'ok' | 'bad' | 'warn' | 'idle'; children: ReactNode }) {
  const color = tone === 'ok' ? C.ok : tone === 'bad' ? C.bad : tone === 'warn' ? C.warn : C.dim;
  return (
    <View style={[s.pill, { borderColor: color }]}>
      <Text style={[s.pillText, { color }]}>{children}</Text>
    </View>
  );
}

export function Btn({
  label,
  onPress,
  busy,
  disabled,
  tone = 'default',
}: {
  label: string;
  onPress: () => void;
  busy?: boolean;
  disabled?: boolean;
  tone?: 'default' | 'quiet' | 'danger';
}) {
  const bg = tone === 'quiet' ? 'transparent' : tone === 'danger' ? '#3d1d1d' : C.accent;
  const fg = tone === 'default' ? '#04101f' : tone === 'danger' ? C.bad : C.dim;
  return (
    <Pressable
      onPress={onPress}
      disabled={busy || disabled}
      style={({ pressed }) => [
        s.btn,
        { backgroundColor: bg, opacity: busy || disabled ? 0.45 : pressed ? 0.75 : 1 },
        tone !== 'default' && { borderWidth: StyleSheet.hairlineWidth, borderColor: C.border },
      ]}>
      {busy ? <ActivityIndicator size="small" color={fg} /> : <Text style={[s.btnText, { color: fg }]}>{label}</Text>}
    </Pressable>
  );
}

export function Card({
  n,
  title,
  question,
  tone,
  status,
  children,
}: {
  n: string;
  title: string;
  question?: string;
  tone: 'ok' | 'bad' | 'warn' | 'idle';
  status: string;
  children: ReactNode;
}) {
  return (
    <View style={s.card}>
      <View style={s.cardHead}>
        <Text style={s.cardN}>{n}</Text>
        <Text style={s.cardTitle}>{title}</Text>
        <Pill tone={tone}>{status}</Pill>
      </View>
      {question ? <Text style={s.question}>{question}</Text> : null}
      <View style={s.cardBody}>{children}</View>
    </View>
  );
}

export function KV({ k, v }: { k: string; v: string }) {
  return (
    <View style={s.kv}>
      <Text style={s.k}>{k}</Text>
      <Text style={s.v} selectable>
        {v}
      </Text>
    </View>
  );
}

export function Verdict({ ok, text }: { ok: boolean | null; text: string }) {
  if (ok === null) return null;
  return (
    <View style={[s.verdict, { borderLeftColor: ok ? C.ok : C.bad }]}>
      <Text style={[s.verdictText, { color: ok ? C.ok : C.bad }]}>{text}</Text>
    </View>
  );
}

export function Row({ children }: { children: ReactNode }) {
  return <View style={s.row}>{children}</View>;
}

/**
 * Two signals side by side. The bar animates on the native driver, so it keeps
 * moving even while JS is stuck; the tick counter is pure JS and stops dead.
 * Together they show whether a block hits the JS thread only or the UI too.
 */
export function LivenessBar() {
  const [frames, setFrames] = useState(0);
  const x = useRef(new Animated.Value(0)).current;

  useEffect(() => {
    const id = setInterval(() => setFrames((f) => f + 1), 50);
    const loop = Animated.loop(
      Animated.timing(x, { toValue: 1, duration: 1400, easing: Easing.linear, useNativeDriver: true })
    );
    loop.start();
    return () => {
      clearInterval(id);
      loop.stop();
    };
  }, [x]);

  const translateX = x.interpolate({ inputRange: [0, 1], outputRange: [0, 240] });
  return (
    <View style={s.liveWrap}>
      <View style={s.liveTrack}>
        <Animated.View style={[s.liveDot, { transform: [{ translateX }] }]} />
      </View>
      <Text style={s.liveText}>js ticks {frames}</Text>
    </View>
  );
}

const s = StyleSheet.create({
  pill: {
    borderWidth: StyleSheet.hairlineWidth,
    borderRadius: 999,
    paddingHorizontal: 8,
    paddingVertical: 2,
  },
  pillText: { fontSize: 10, fontWeight: '700', letterSpacing: 0.6, textTransform: 'uppercase' },
  btn: {
    borderRadius: 8,
    paddingHorizontal: 14,
    paddingVertical: 9,
    minWidth: 84,
    alignItems: 'center',
    justifyContent: 'center',
  },
  btnText: { fontSize: 13, fontWeight: '600' },
  card: {
    backgroundColor: C.panel,
    borderRadius: 12,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: C.border,
    marginBottom: 14,
    overflow: 'hidden',
  },
  cardHead: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 8,
    paddingHorizontal: 14,
    paddingTop: 13,
  },
  cardN: { color: C.accent, fontFamily: mono, fontSize: 12, fontWeight: '700' },
  cardTitle: { color: C.text, fontSize: 15, fontWeight: '600', flex: 1 },
  question: { color: C.dim, fontSize: 12, paddingHorizontal: 14, paddingTop: 6, lineHeight: 17 },
  cardBody: { padding: 14, gap: 10 },
  kv: { flexDirection: 'row', gap: 10, alignItems: 'flex-start' },
  k: { color: C.dim, fontFamily: mono, fontSize: 11, width: 118 },
  v: { color: C.text, fontFamily: mono, fontSize: 11, flex: 1, lineHeight: 16 },
  verdict: {
    borderLeftWidth: 3,
    paddingLeft: 10,
    paddingVertical: 4,
    backgroundColor: C.panelAlt,
    borderRadius: 4,
  },
  verdictText: { fontSize: 12.5, lineHeight: 18, fontWeight: '500' },
  row: { flexDirection: 'row', gap: 8, alignItems: 'center', flexWrap: 'wrap' },
  liveWrap: { gap: 4 },
  liveTrack: {
    height: 4,
    borderRadius: 2,
    backgroundColor: C.panelAlt,
    width: 252,
    justifyContent: 'center',
  },
  liveDot: { width: 12, height: 4, borderRadius: 2, backgroundColor: C.accent },
  liveText: { color: C.dim, fontFamily: mono, fontSize: 10 },
});

export const shared = s;
