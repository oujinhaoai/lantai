import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

const key = "lantai.ui.file-preview-ratio.v1";
const defaultRatio = 0.4;
const minimumRatio = 0.1;
const maximumRatio = 0.9;
const clamp = (value: number, min: number, max: number) =>
  Math.max(min, Math.min(max, value));
function limits(width: number) {
  const available = Math.max(1054, width);
  return {
    minimum: Math.max(minimumRatio, 360 / available),
    maximum: Math.min(maximumRatio, (available - 462) / available),
  };
}
function loadRatio() {
  try {
    const raw = localStorage.getItem(key);
    const value = raw === null ? NaN : Number(raw);
    if (
      Number.isFinite(value) &&
      value >= minimumRatio &&
      value <= maximumRatio
    )
      return value;
  } catch {
    // This optional layout preference must never prevent reading a resource.
  }
  return defaultRatio;
}
function saveRatio(value: number | null) {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, String(value));
  } catch {
    // In-memory resizing still works when browser storage is unavailable.
  }
}
function availabilityStore() {
  let ready = false;
  const listeners = new Set<() => void>();
  return {
    getSnapshot: () => ready,
    subscribe: (listener: () => void) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    update: (next: boolean) => {
      if (next === ready) return;
      ready = next;
      listeners.forEach((listener) => listener());
    },
  };
}

export function FilePreviewSplit({ drawerID }: { drawerID: string }) {
  const [ratio, setRatio] = useState(loadRatio);
  const [workspaceWidth, setWorkspaceWidth] = useState(
    () => window.innerWidth - 226,
  );
  const [dragging, setDragging] = useState(false);
  const [availability] = useState(availabilityStore);
  const adjustable = useSyncExternalStore(
    availability.subscribe,
    availability.getSnapshot,
  );
  const handle = useRef<HTMLDivElement>(null);
  const ratioRule = useRef<CSSStyleRule | null>(null);
  const preference = useRef(ratio);
  const drag = useRef<{
    pointerID: number;
    start: number;
    viewport: string;
  } | null>(null);
  const helpID = useId();
  const handleID = useId();
  // Match the CSS constraints. Resizing the window never saves a clamped value.
  const { minimum, maximum } = limits(workspaceWidth);
  const actual = clamp(ratio, minimum, maximum);
  const percent = Math.round(actual * 100);
  const applyRatio = useCallback((value: number) => {
    preference.current = value;
    ratioRule.current?.style.setProperty("--file-drawer-ratio", String(value));
    setRatio(value);
  }, []);
  const change = (next: number, save: boolean) => {
    if (!ratioRule.current) return;
    const value = clamp(next, minimumRatio, maximumRatio);
    applyRatio(value);
    if (save) saveRatio(value);
  };
  const finish = useCallback(
    (commit: boolean) => {
      const active = drag.current;
      if (!active) return;
      drag.current = null;
      setDragging(false);
      if (commit) saveRatio(preference.current);
      else applyRatio(active.start);
      if (handle.current?.hasPointerCapture(active.pointerID))
        handle.current.releasePointerCapture(active.pointerID);
    },
    [applyRatio],
  );
  useLayoutEffect(() => {
    // Use the app's existing same-origin external sheet: no inline style
    // attributes/elements and no relaxation of the gateway's style-src policy.
    for (const sheet of Array.from(document.styleSheets)) {
      try {
        const developmentStyle =
          import.meta.env.DEV &&
          !sheet.href &&
          sheet.ownerNode instanceof HTMLStyleElement &&
          sheet.ownerNode.hasAttribute("data-vite-dev-id");
        if (
          sheet.disabled ||
          (!developmentStyle &&
            (!sheet.href ||
              new URL(sheet.href).origin !== window.location.origin)) ||
          (sheet.media.mediaText && !matchMedia(sheet.media.mediaText).matches)
        )
          continue;
        const index = sheet.insertRule(
          `main:has(#${CSS.escape(handleID)}) { --file-drawer-ratio: ${preference.current}; }`,
          sheet.cssRules.length,
        );
        const rule = sheet.cssRules[index] as CSSStyleRule;
        ratioRule.current = rule;
        availability.update(true);
        return () => {
          ratioRule.current = null;
          try {
            const current = Array.from(sheet.cssRules).indexOf(rule);
            if (current >= 0) sheet.deleteRule(current);
          } catch {
            // The stylesheet may already have been removed during navigation.
          }
        };
      } catch {
        // Try another enabled same-origin sheet; never fall back to inline CSS.
      }
    }
    availability.update(false);
  }, [handleID, availability]);
  useLayoutEffect(() => {
    const workspace = handle.current?.closest(".workspace");
    if (!workspace) return;
    const measure = () => {
      setWorkspaceWidth(workspace.getBoundingClientRect().width);
    };
    const observer = new ResizeObserver(measure);
    observer.observe(workspace);
    measure();
    return () => {
      observer.disconnect();
    };
  }, []);
  useEffect(() => {
    const element = handle.current;
    const resize = () => {
      finish(false);
    };
    window.addEventListener("resize", resize);
    return () => {
      window.removeEventListener("resize", resize);
      const active = drag.current;
      drag.current = null;
      if (active && element?.hasPointerCapture(active.pointerID))
        element.releasePointerCapture(active.pointerID);
    };
  }, [finish]);
  const reset = () => {
    if (!ratioRule.current) return;
    finish(false);
    change(defaultRatio, false);
    saveRatio(null);
  };
  return (
    <>
      <div
        ref={handle}
        id={handleID}
        className="preview-split-handle"
        role="separator"
        tabIndex={adjustable ? 0 : -1}
        aria-disabled={!adjustable}
        aria-label="调整文件列表与预览比例"
        aria-orientation="vertical"
        aria-controls={drawerID}
        aria-describedby={helpID}
        aria-valuemin={Math.round(minimum * 100)}
        aria-valuemax={Math.round(maximum * 100)}
        aria-valuenow={percent}
        aria-valuetext={`文件列表 ${100 - percent}%，预览 ${percent}%`}
        title="左右拖动调节比例，也可使用方向键"
        data-resizing={dragging}
        onPointerDown={(event) => {
          if (
            event.button !== 0 ||
            !event.isPrimary ||
            !ratioRule.current ||
            window.innerWidth < 1280
          )
            return;
          event.preventDefault();
          event.currentTarget.focus();
          event.currentTarget.setPointerCapture(event.pointerId);
          drag.current = {
            pointerID: event.pointerId,
            start: preference.current,
            viewport: `${window.innerWidth}x${window.innerHeight}`,
          };
          setDragging(true);
        }}
        onPointerMove={(event) => {
          if (drag.current?.pointerID !== event.pointerId) return;
          if (
            window.innerWidth < 1280 ||
            drag.current.viewport !==
              `${window.innerWidth}x${window.innerHeight}`
          ) {
            finish(false);
            return;
          }
          event.preventDefault();
          const workspace = event.currentTarget
            .closest(".workspace")!
            .getBoundingClientRect();
          change((workspace.right - event.clientX) / workspace.width, false);
        }}
        onPointerUp={(event) => {
          if (drag.current?.pointerID !== event.pointerId) return;
          event.preventDefault();
          finish(
            window.innerWidth >= 1280 &&
              drag.current.viewport ===
                `${window.innerWidth}x${window.innerHeight}`,
          );
        }}
        onPointerCancel={(event) => {
          if (drag.current?.pointerID === event.pointerId) finish(false);
        }}
        onLostPointerCapture={() => finish(false)}
        onClick={(event) => {
          event.preventDefault();
          event.stopPropagation();
        }}
        onDoubleClick={reset}
        onKeyDown={(event) => {
          if (!ratioRule.current) return;
          if (event.key === "Escape" && drag.current) {
            event.preventDefault();
            event.stopPropagation();
            finish(false);
            return;
          }
          if (drag.current) {
            if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key))
              event.preventDefault();
            return;
          }
          const bounds = limits(
            event.currentTarget.closest(".workspace")!.getBoundingClientRect()
              .width,
          );
          const current = clamp(
            preference.current,
            bounds.minimum,
            bounds.maximum,
          );
          let next: number;
          const step = event.shiftKey ? 0.1 : 0.02;
          if (event.key === "ArrowLeft")
            next = Math.min(bounds.maximum, current + step);
          else if (event.key === "ArrowRight")
            next = Math.max(bounds.minimum, current - step);
          else if (event.key === "Home") next = minimumRatio;
          else if (event.key === "End") next = maximumRatio;
          else return;
          event.preventDefault();
          if (
            Math.abs(clamp(next, bounds.minimum, bounds.maximum) - current) >
            0.000001
          )
            change(next, true);
        }}
      />
      <div className="preview-split-controls">
        <p className="subtle" id={helpID}>
          {adjustable
            ? "拖动分隔条调整比例，也可用左右方向键"
            : "此浏览器暂时无法调整比例，已使用默认布局"}
        </p>
        <button className="text-button" onClick={reset} disabled={!adjustable}>
          恢复默认比例
        </button>
      </div>
    </>
  );
}
