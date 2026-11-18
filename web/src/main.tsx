import "@fontsource-variable/archivo/wdth.css";
import "./styles.css";
import { createRoot } from "react-dom/client";
import App from "./App";

const container = document.getElementById("app");
if (!container) {
  throw new Error("Missing #app mount point");
}
createRoot(container).render(<App />);
