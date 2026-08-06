import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./App";
import { SharePage } from "./SharePage";
import "./styles.css";

const root = document.getElementById("root");
if (!root) throw new Error("#root is missing from index.html");

// The public share plane is chosen at the entry, not inside App: a /s/{token}
// page must render with no session and none of the app's authenticated chrome,
// and on the public host that is the only route that exists.
const path = window.location.pathname;
const shareRoute = path.startsWith("/s/") ? decodeURIComponent(path.slice(3)) : null;

createRoot(root).render(
  <StrictMode>{shareRoute ? <SharePage token={shareRoute} /> : <App />}</StrictMode>,
);
