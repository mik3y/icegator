import "@fontsource-variable/archivo/wdth.css";
import "./styles.css";
import { createRoot } from "react-dom/client";
import App from "./App";

const container = document.getElementById("app");
if (!container) {
  throw new Error("Missing #app mount point");
}
createRoot(container).render(<App />);

if (import.meta.env.MODE === "sim") {
  // The simulator's controls sit outside the app, in a root of their own.
  import("./sim/Toolbar").then(({ default: Toolbar }) => {
    const bar = document.body.appendChild(document.createElement("div"));
    createRoot(bar).render(<Toolbar />);
  });
}
