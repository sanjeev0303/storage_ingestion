"use client";

import gsap from "gsap";
import { copy } from "../copy";
import {
  DiagramFigure,
  Mark,
  useDiagramRef,
  useDiagramTimeline,
} from "../diagram-kit";

const PART = 44;
const LANE_X = [75, 165, 255, 345];
const SLOT_Y = [130, 232];
const FILE_Y = 38;
const BLOCK = { x: 34, y: 330, w: 352, h: 84 };
const RETRIED = 4; // the fifth part

const parts = Array.from({ length: 8 }, (_, i) => ({
  index: i,
  x: LANE_X[i % 4],
  y: SLOT_Y[Math.floor(i / 4)],
  fileX: 56 + 44 * i,
}));

const fillStyle = {
  transformBox: "fill-box",
  transformOrigin: "50% 100%",
} as const;

export function MultipartUpload() {
  const ref = useDiagramRef();

  useDiagramTimeline(ref, (tl, root) => {
    const q = (selector: string) =>
      gsap.utils.toArray<SVGElement>(selector, root);
    const fills = q("[data-fill]");
    const retriedFill = fills[RETRIED];
    const otherSecondRow = fills.slice(4).filter((_, i) => i + 4 !== RETRIED);
    const failMark = q("[data-fail]");
    const retryLabel = q("[data-retry]");
    const reload = q("[data-reload]");
    const fileLabel = q("[data-file-label]");
    const blockParts = q("[data-part]");

    tl.to(fileLabel, { autoAlpha: 0, duration: 0.2 }, 0);
    tl.from(blockParts, {
      x: (i) => parts[i].fileX - parts[i].x,
      y: (i) => FILE_Y - parts[i].y,
      duration: 0.7,
      ease: "power3.out",
      stagger: 0.05,
    });
    tl.to(fileLabel, { autoAlpha: 1, duration: 0.3 }, ">-0.1");
    tl.from(
      fills.slice(0, 4),
      { scaleY: 0, duration: 0.9, stagger: 0.08 },
      "-=0.1",
    );

    tl.from(reload, { autoAlpha: 0, duration: 0.3 }, "+=0.1");
    tl.to({}, { duration: 0.8 });

    tl.addLabel("second");
    tl.from(
      otherSecondRow,
      { scaleY: 0, duration: 0.9, stagger: 0.08 },
      "second",
    );
    tl.fromTo(
      retriedFill,
      { scaleY: 0 },
      { scaleY: 0.55, duration: 0.5 },
      "second",
    );
    tl.fromTo(
      failMark,
      { autoAlpha: 0 },
      { autoAlpha: 1, duration: 0.15 },
      "second+=0.55",
    );
    tl.to(
      blockParts[RETRIED],
      { x: 5, duration: 0.06, repeat: 5, yoyo: true },
      "second+=0.55",
    );
    tl.fromTo(
      retryLabel,
      { autoAlpha: 0 },
      { autoAlpha: 1, duration: 0.2 },
      ">+0.1",
    );
    tl.to(failMark, { autoAlpha: 0, duration: 0.15 }, "<");
    tl.to(retriedFill, { scaleY: 0, duration: 0.2 }, "<");
    tl.to(retriedFill, { scaleY: 1, duration: 0.9 });

    tl.addLabel("join", "+=0.3");
    tl.to(retryLabel, { autoAlpha: 0, duration: 0.2 }, "join");
    tl.to(
      blockParts,
      {
        x: (i) => BLOCK.x + BLOCK.w / 2 - parts[i].x,
        y: (i) => BLOCK.y + BLOCK.h / 2 - parts[i].y,
        duration: 0.8,
        ease: "power2.inOut",
        stagger: 0.04,
      },
      "join",
    );
    tl.to(blockParts, { autoAlpha: 0, duration: 0.2 }, ">-0.2");
    tl.from("[data-object]", { autoAlpha: 0, y: 8, duration: 0.4 }, "<");
  });

  return (
    <DiagramFigure label={copy.upload.diagramLabel}>
      <div ref={ref}>
        <svg
          viewBox="0 0 420 470"
          className="block h-auto w-full"
          focusable="false"
        >
          <g>
            <rect
              x="34"
              y="16"
              width="352"
              height="44"
              fill="var(--faint)"
              stroke="currentColor"
              strokeWidth="2"
            />
            <text data-file-label x="48" y="44" fontSize="15">
              raw/session-08.bin
            </text>
            <text data-file-label x="372" y="44" textAnchor="end" fontSize="15">
              1.8 GiB
            </text>
          </g>

          <text x="210" y="90" textAnchor="middle" fontSize="15">
            8 parts, 4 at a time
          </text>

          {LANE_X.map((x) => (
            <rect
              key={x}
              x={x - 28}
              y="104"
              width="56"
              height="152"
              fill="var(--faint)"
            />
          ))}

          <g data-reload>
            <line
              x1="20"
              x2="400"
              y1="181"
              y2="181"
              stroke="currentColor"
              strokeWidth="2"
              strokeDasharray="6 5"
            />
            <text x="398" y="172" textAnchor="end" fontSize="14">
              reload
            </text>
          </g>

          {parts.map((part) => (
            <g key={part.index} transform={`translate(${part.x} ${part.y})`}>
              <g data-part>
                <rect
                  x={-PART / 2}
                  y={-PART / 2}
                  width={PART}
                  height={PART}
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="2"
                />
                <rect
                  data-fill
                  x={-PART / 2}
                  y={-PART / 2}
                  width={PART}
                  height={PART}
                  fill="currentColor"
                  style={fillStyle}
                />
              </g>
            </g>
          ))}

          <g
            transform={`translate(${parts[RETRIED].x} ${parts[RETRIED].y})`}
            opacity="0"
            data-fail
          >
            <Mark kind="failed" x={-12} y={-12} size={24} />
          </g>
          <text
            x={parts[RETRIED].x}
            y="274"
            textAnchor="middle"
            fontSize="15"
            opacity="0"
            data-retry
          >
            retry
          </text>

          <text x="210" y="300" textAnchor="middle" fontSize="14">
            confirmed parts are skipped after a reload
          </text>

          <g data-object>
            <rect
              x={BLOCK.x}
              y={BLOCK.y}
              width={BLOCK.w}
              height={BLOCK.h}
              fill="var(--faint)"
              stroke="currentColor"
              strokeWidth="2"
            />
            <text
              x={BLOCK.x + 20}
              y={BLOCK.y + 34}
              fontSize="18"
              fontWeight="600"
            >
              Object
            </text>
            <text
              className="mono"
              x={BLOCK.x + 20}
              y={BLOCK.y + 62}
              fontSize="15"
            >
              {copy.ingest.entry.checksum}
            </text>
            <text x="210" y="446" textAnchor="middle" fontSize="15">
              8 of 8 parts confirmed
            </text>
          </g>
        </svg>
      </div>
    </DiagramFigure>
  );
}
