"use client";

import { useGSAP } from "@gsap/react";
import gsap from "gsap";
import { useRef, type RefObject } from "react";
import { copy } from "../copy";
import { DiagramFigure, Mark } from "../diagram-kit";

gsap.registerPlugin(useGSAP);

type Member = {
  id: string;
  x: number;
  y: number;
  size: number;
  marked?: boolean;
};

function MemberSquare({ id, x, y, size, marked }: Member) {
  return (
    <g>
      <rect
        x={x}
        y={y}
        width={size}
        height={size}
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
      />
      <text
        x={x + size / 2}
        y={y + size / 2 + 4.5}
        textAnchor="middle"
        fontSize="13"
        className="mono"
      >
        {id}
      </text>
      {marked && (
        <rect
          x={x + size - 9}
          y={y - 5}
          width="10"
          height="10"
          fill="var(--accent)"
          stroke="currentColor"
          strokeWidth="1.5"
        />
      )}
    </g>
  );
}

function VersionBox({
  name,
  hash,
  y,
  members,
  frozen,
}: {
  name: string;
  hash: string;
  y: number;
  members: Member[];
  frozen?: boolean;
}) {
  return (
    <g>
      <rect
        x="20"
        y={y}
        width="380"
        height="110"
        fill="var(--ground)"
        stroke="currentColor"
        strokeWidth="2"
      />
      <text x="36" y={y + 30} fontSize="18" fontWeight="600">
        {name}
      </text>
      <Mark kind="passed" x={262} y={y + 15} />
      <text x="284" y={y + 28} fontSize="14" fontWeight="600">
        FINALIZED
      </text>
      <text x="36" y={y + 58} fontSize="14" className="mono">
        {hash}
      </text>
      <text x="384" y={y + 58} fontSize="13" textAnchor="end">
        Example
      </text>
      {members.map((member) => (
        <MemberSquare key={member.id} {...member} />
      ))}
      {frozen && (
        <text data-frozen x="384" y={y + 92} fontSize="14" textAnchor="end">
          Frozen
        </text>
      )}
    </g>
  );
}

function GhostBox({
  name,
  y,
  ghost,
}: {
  name: string;
  y: number;
  ghost: string;
}) {
  return (
    <g {...{ [ghost]: "" }} className="opacity-0">
      <rect
        x="20"
        y={y}
        width="380"
        height="110"
        stroke="currentColor"
        strokeWidth="2"
        strokeDasharray="6 5"
      />
      <text x="36" y={y + 30} fontSize="18" fontWeight="600">
        {name}
      </text>
      <text x="36" y={y + 58} fontSize="14">
        Not pinned yet
      </text>
    </g>
  );
}

const row = (y: number, ids: string[], size = 28) =>
  ids.map((id, index) => ({
    id,
    x: 36 + index * (size + 8),
    y,
    size,
    marked: id === "m4",
  }));

// The timeline is 3 seconds long, one second per stage, and is driven by the
// slider's progress. The markup is the last stage, so without script it shows
// the finished picture.
export function SnapshotDraft({
  timelineRef,
}: {
  timelineRef: RefObject<gsap.core.Timeline | null>;
}) {
  const scope = useRef<HTMLDivElement>(null);

  useGSAP(
    () => {
      const tl = gsap.timeline({
        paused: true,
        defaults: { ease: "power2.out", duration: 0.8 },
      });
      // The dashed placeholders turn into the solid boxes as each version is pinned.
      tl.from("[data-v1]", { autoAlpha: 0, y: 20 }, 0)
        .from("[data-line]", { autoAlpha: 0 }, 0)
        .fromTo(
          "[data-ghost1], [data-ghost-line]",
          { autoAlpha: 1 },
          { autoAlpha: 0 },
          0,
        )
        .fromTo(
          "[data-ghost2]",
          { autoAlpha: 1 },
          { autoAlpha: 0, duration: 1 },
          2,
        )
        .fromTo("[data-m3-old]", { autoAlpha: 1 }, { autoAlpha: 0, y: -10 }, 1)
        .from("[data-m3-new]", { autoAlpha: 0, y: 10 }, 1)
        .from("[data-frozen]", { autoAlpha: 0 }, 1.2)
        .from("[data-v2]", { autoAlpha: 0, y: 20, duration: 1 }, 2);
      timelineRef.current = tl;
      return () => {
        timelineRef.current = null;
      };
    },
    { scope },
  );

  return (
    <div ref={scope}>
      <DiagramFigure label={copy.snapshot.diagramLabel}>
        <svg viewBox="0 0 420 404" className="block h-auto w-full" fill="none">
          <rect
            x="20"
            y="12"
            width="380"
            height="104"
            fill="var(--ground)"
            stroke="currentColor"
            strokeWidth="2"
          />
          <text x="36" y="42" fontSize="18" fontWeight="600">
            Draft
          </text>
          <text x="384" y="42" fontSize="14" textAnchor="end">
            Members can change
          </text>
          <MemberSquare id="m1" x={36} y={62} size={36} />
          <MemberSquare id="m2" x={84} y={62} size={36} />
          <g data-m3-old className="opacity-0">
            <MemberSquare id="m3" x={132} y={62} size={36} />
          </g>
          <g data-m3-new>
            <MemberSquare id="m4" x={132} y={62} size={36} marked />
          </g>

          <path
            data-line
            d="M210 116v24"
            stroke="currentColor"
            strokeWidth="2"
          />
          <path
            data-ghost-line
            className="opacity-0"
            d="M210 116v24"
            stroke="currentColor"
            strokeWidth="2"
            strokeDasharray="4 4"
          />
          <GhostBox name="Version 1" y={140} ghost="data-ghost1" />
          <GhostBox name="Version 2" y={282} ghost="data-ghost2" />

          <g data-v1>
            <VersionBox
              name="Version 1"
              hash="manifest 4e1a…9b20"
              y={140}
              members={row(212, ["m1", "m2", "m3"])}
              frozen
            />
          </g>
          <g data-v2>
            <VersionBox
              name="Version 2"
              hash="manifest b83d…1f65"
              y={282}
              members={row(354, ["m1", "m2", "m4"])}
            />
          </g>
        </svg>
      </DiagramFigure>
    </div>
  );
}
