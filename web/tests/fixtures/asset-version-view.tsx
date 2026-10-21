// Test-only component host. The default production build does not include
// this HTML entry. Requests in asset-versions.spec.ts are synthetic responses;
// this fixed CSRF value cannot authenticate against a real instance.
import { createRoot } from "react-dom/client";
import { useCallback, useState, useSyncExternalStore } from "react";
import { AssetVersionView } from "../../src/asset-version-view";
import { rememberCSRF } from "../../src/api";
import "../../src/style.css";

rememberCSRF("synthetic-session", "synthetic-csrf");
const snapshot = () => location.hash.slice(1) || "/";
const subscribe = (change: () => void) => {
  addEventListener("hashchange", change);
  return () => removeEventListener("hashchange", change);
};
function Fixture() {
  const route = useSyncExternalStore(subscribe, snapshot);
  const [expired, setExpired] = useState(false);
  const expire = useCallback(() => setExpired(true), []);
  return expired ? (
    <p role="alert">合成会话已过期</p>
  ) : (
    <main id="main">
      <AssetVersionView
        assetID={route.split("?")[0].slice(8)}
        route={route}
        expire={expire}
      />
    </main>
  );
}
createRoot(document.getElementById("root")!).render(<Fixture />);
