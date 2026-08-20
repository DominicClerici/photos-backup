"use client";

import { HardDrive } from "lucide-react";
import { Cell, Label, Pie, PieChart } from "recharts";

import type { StorageStatus } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { breakdown, percentUsed, type StorageRow } from "@/lib/storage";
import { Card } from "@/components/ui/card";
import { ChartContainer, ChartTooltip, type ChartConfig } from "@/components/ui/chart";

const config = {
  used: { label: "Used", color: "var(--color-chart-1)" },
  free: { label: "Free", color: "var(--color-chart-4)" },
} satisfies ChartConfig;

/**
 * The drive, as one ring.
 *
 * Two segments and no more, because the question the card answers from across
 * the room is "am I going to run out" — and a five-slice pie answers it worse
 * than a two-slice one. The composition of the used half is a real question and
 * a second one, so it lives in the hover card: available to anyone who asks,
 * and never in the way of the first answer.
 */
export function StorageCard({ storage }: { storage: StorageStatus }) {
  const b = breakdown(storage);
  const percent = percentUsed(b.total, b.used);

  // A drive photod could not stat — no PHOTOS_ROOT on this host, or a mount
  // that went away. The card says which rather than drawing a ring of zeroes.
  if (b.total <= 0) {
    return (
      <Card className="gap-3 px-4">
        <Header />
        <p className="pb-1 text-[13px] text-muted-foreground">
          The archive volume could not be read.
        </p>
      </Card>
    );
  }

  const data = [
    { key: "used", value: b.used, fill: "var(--color-used)" },
    { key: "free", value: b.free, fill: "var(--color-free)" },
  ];

  return (
    // The ring's hover card is drawn by recharts inside this card, and the card
    // clips its children by default so that an image can sit flush in its
    // corners. A breakdown taller than the card it hangs off is exactly the
    // case that would be cut in half.
    <Card className="gap-2 overflow-visible px-4">
      <Header />

      <div className="flex items-center gap-3">
        <ChartContainer config={config} className="aspect-square size-[112px] shrink-0">
          <PieChart>
            <ChartTooltip
              cursor={false}
              // Free to leave the 112px square it is drawn in, which is the
              // only way a five-line breakdown fits beside a ring that small.
              allowEscapeViewBox={{ x: true, y: true }}
              offset={14}
              wrapperStyle={{ zIndex: 30, pointerEvents: "none" }}
              content={<StorageTooltip breakdown={b} />}
            />
            <Pie
              data={data}
              dataKey="value"
              nameKey="key"
              innerRadius={38}
              outerRadius={54}
              startAngle={90}
              endAngle={-270}
              paddingAngle={2}
              cornerRadius={3}
              strokeWidth={0}
              isAnimationActive={false}
            >
              {data.map((slice) => (
                <Cell
                  key={slice.key}
                  fill={slice.fill}
                  className="outline-none transition-opacity hover:opacity-85"
                />
              ))}
              <Label
                content={({ viewBox }) => {
                  if (!viewBox || !("cx" in viewBox)) return null;
                  const { cx, cy } = viewBox as { cx: number; cy: number };
                  return (
                    <text x={cx} y={cy} textAnchor="middle" dominantBaseline="middle">
                      <tspan
                        x={cx}
                        y={cy - 5}
                        className="fill-foreground text-[15px] font-semibold tabular-nums"
                      >
                        {formatBytes(b.used)}
                      </tspan>
                      <tspan x={cx} y={cy + 11} className="fill-faint text-[11px] tabular-nums">
                        of {formatBytes(b.total)}
                      </tspan>
                    </text>
                  );
                }}
              />
            </Pie>
          </PieChart>
        </ChartContainer>

        <div className="flex min-w-0 flex-col gap-1.5">
          <div className="flex items-baseline gap-1.5">
            <span className="text-[26px] leading-none font-semibold tabular-nums">{percent}%</span>
            <span className="text-[13px] text-faint">used</span>
          </div>
          <p className="text-[13px] text-muted-foreground">
            <span className="tabular-nums">{formatBytes(b.free)}</span> free
          </p>
          <p className="text-[11px] text-faint">Hover the ring for a breakdown</p>
        </div>
      </div>
    </Card>
  );
}

function Header() {
  return (
    <div className="flex items-center gap-2.5">
      <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-tile">
        <HardDrive className="size-4 text-muted-foreground" aria-hidden="true" />
      </span>
      <span className="flex-1 truncate text-[13px] font-medium tracking-[0.01em]">Storage</span>
    </div>
  );
}

/**
 * What the used half is made of.
 *
 * Recharts hands the hovered slice through `payload`, so this is one component
 * answering two questions: hovering the free segment is asking about free
 * space, and listing the archive's contents there would be answering something
 * nobody asked.
 */
function StorageTooltip({
  breakdown: b,
  active,
  payload,
}: {
  breakdown: ReturnType<typeof breakdown>;
  active?: boolean;
  payload?: { payload?: { key?: string } }[];
}) {
  if (!active || !payload?.length) return null;
  const key = payload[0]?.payload?.key;

  return (
    <div className="min-w-[212px] rounded-lg border bg-card/95 px-3 py-2.5 text-xs shadow-xl backdrop-blur-md">
      {key === "free" ? (
        <>
          <p className="font-medium">Free space</p>
          <p className="mt-1 text-[11px] text-muted-foreground">
            <span className="tabular-nums">{formatBytes(b.free)}</span> left of{" "}
            <span className="tabular-nums">{formatBytes(b.total)}</span> —{" "}
            {100 - percentUsed(b.total, b.used)}% of the drive.
          </p>
        </>
      ) : (
        <>
          <p className="mb-2 flex items-baseline justify-between gap-4 font-medium">
            <span>In use</span>
            <span className="tabular-nums text-muted-foreground">{formatBytes(b.used)}</span>
          </p>
          <dl className="flex flex-col gap-1">
            {b.rows.map((row) => (
              <Row key={row.key} row={row} />
            ))}
          </dl>
          <p className="mt-2 border-t pt-2 text-[10.5px] leading-snug text-faint">
            Everything else is the database, the vault, and the space the filesystem holds back.
          </p>
          {b.elsewhere.length > 0 ? (
            <>
              <p className="mt-2.5 mb-1 border-t pt-2 text-[11px] text-faint">
                On another disk, so not in this ring
              </p>
              <dl className="flex flex-col gap-1">
                {b.elsewhere.map((row) => (
                  <Row key={row.key} row={row} />
                ))}
              </dl>
            </>
          ) : null}
        </>
      )}
    </div>
  );
}

function Row({ row }: { row: StorageRow }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <dt className="text-muted-foreground">{row.label}</dt>
      <dd className="shrink-0 tabular-nums">{formatBytes(row.bytes)}</dd>
    </div>
  );
}
