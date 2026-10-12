import React from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./style.css";

/** 界面出错时显示原因和「重新加载」，而不是整页空白。 */
class ErrorBoundary extends React.Component<{ children: React.ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="crash">
        <h3>界面出错了</h3>
        <pre>{String(this.state.error?.message || this.state.error)}</pre>
        <button className="primary" onClick={() => location.reload()}>重新加载</button>
      </div>
    );
  }
}

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary>
      <App />
    </ErrorBoundary>
  </React.StrictMode>,
);
