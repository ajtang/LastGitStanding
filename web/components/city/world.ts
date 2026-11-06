import * as THREE from "three";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";
import { EffectComposer } from "three/addons/postprocessing/EffectComposer.js";
import { RenderPass } from "three/addons/postprocessing/RenderPass.js";
import { UnrealBloomPass } from "three/addons/postprocessing/UnrealBloomPass.js";
import { OutputPass } from "three/addons/postprocessing/OutputPass.js";
import type { Frame, RegionHealth, RegionState } from "@/lib/types";
import { Layer, type Placement } from "./layer";

// Fixed sites, so a region stays where you left it when others come and go.
const SITES: Record<string, [number, number]> = {
  eastus: [0, 0],
  westeurope: [-86, -26],
  southeastasia: [86, -26],
  westus2: [-50, 66],
  uksouth: [52, 66],
};

const SLOT = 2.6; // one pod's footprint
const SLOTS_PER_SIDE: Record<string, number> = { small: 2, standard: 2, large: 4 };
const SLOTS_DEEP: Record<string, number> = { small: 1, standard: 2, large: 4 };
const NODE_GAP = 1.1;

const C = {
  plate: new THREE.Color("#35455f"),
  plateDown: new THREE.Color("#0d0f13"),
  tile: new THREE.Color("#51658a"),
  tileBooting: new THREE.Color("#57502d"),
  road: new THREE.Color("#3a4557"),
  calm: new THREE.Color("#79d3ff"),
  busy: new THREE.Color("#ffc65c"),
  hot: new THREE.Color("#ff4b3e"),
  starting: new THREE.Color("#b9c4d6"),
  dark: new THREE.Color("#10141b"),
  ghost: new THREE.Color("#55709a"),
  house: new THREE.Color("#ffdca8"),
  cacheOn: new THREE.Color("#7df0c4"),
  standby: new THREE.Color("#8d7cf0"),
  headlight: new THREE.Color("#d9b77f"),
  failing: new THREE.Color("#ff3b30"),
  flow: new THREE.Color("#6fe3ff"),
  dusk: new THREE.Color("#1a2a44"),
  night: new THREE.Color("#060910"),
  alarm: new THREE.Color("#3a0f12"),
};

// Cool when idle, amber near capacity, red when it's over. Readable from across the room.
function heat(u: number): THREE.Color {
  if (u < 0.65) return C.calm;
  if (u < 0.9) return C.calm.clone().lerp(C.busy, (u - 0.65) / 0.25);
  return C.busy.clone().lerp(C.hot, Math.min(1, (u - 0.9) / 0.25));
}

function hash(n: number): number {
  const s = Math.sin(n * 127.1) * 43758.5453;
  return s - Math.floor(s);
}

function windowTexture(): THREE.CanvasTexture {
  const size = 128;
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = size;
  const g = canvas.getContext("2d")!;
  g.fillStyle = "#000";
  g.fillRect(0, 0, size, size);
  for (let y = 8; y < size - 6; y += 12) {
    for (let x = 8; x < size - 6; x += 12) {
      const lit = hash(x * 13 + y) > 0.28;
      g.fillStyle = lit ? `rgba(255,255,255,${0.55 + hash(x + y * 7) * 0.45})` : "#050505";
      g.fillRect(x, y, 7, 8);
    }
  }
  const tex = new THREE.CanvasTexture(canvas);
  tex.colorSpace = THREE.SRGBColorSpace;
  return tex;
}

// Dark body, windows glowing in the instance color. Three's stock materials tint the
// whole surface with instanceColor, and a city of solid-blue boxes reads as a chart.
function litBuildingMaterial(tex: THREE.Texture): THREE.MeshStandardMaterial {
  const m = new THREE.MeshStandardMaterial({
    color: 0xffffff, roughness: 0.75, metalness: 0.1,
    emissive: 0xffffff, emissiveIntensity: 0.85, emissiveMap: tex,
  });
  m.onBeforeCompile = (shader) => {
    shader.fragmentShader = shader.fragmentShader
      .replace("#include <color_fragment>", "#include <color_fragment>\n diffuseColor.rgb = mix(vec3(0.05,0.065,0.09), vColor.rgb, 0.05);")
      .replace("#include <emissivemap_fragment>", "#include <emissivemap_fragment>\n totalEmissiveRadiance *= vColor.rgb;");
  };
  return m;
}

function boxWithBlankRoof(): THREE.BoxGeometry {
  const geo = new THREE.BoxGeometry(1, 1, 1);
  // Faces 2 and 3 are +y/-y. Pointing their UVs at the texture's black margin keeps
  // windows off the roof, which is a surprisingly loud detail when it's wrong.
  const uv = geo.getAttribute("uv") as THREE.BufferAttribute;
  for (let v = 8; v < 16; v++) uv.setXY(v, 0.01, 0.99);
  return geo;
}

interface RegionLayout {
  name: string; cx: number; cz: number; w: number; d: number;
  entry: THREE.Vector3; gate: THREE.Vector3; core: THREE.Vector3; dbAnchor: THREE.Vector3;
}

interface Vehicle { region: string; t: number; speed: number; failing: boolean; lane: number; lane2: number; dead: boolean }
interface Particle { from: THREE.Vector3; to: THREE.Vector3; t: number; speed: number; failing: boolean; arc: number }

const MAX_VEHICLES = 700;
const MAX_PARTICLES = 260;

export class CityWorld {
  private renderer: THREE.WebGLRenderer;
  private scene = new THREE.Scene();
  private camera: THREE.PerspectiveCamera;
  private controls: OrbitControls;
  private composer: EffectComposer;
  private hemi = new THREE.HemisphereLight(0xa9c4ff, 0x20242e, 1.1);
  private sun = new THREE.DirectionalLight(0xffd9b0, 1.2);
  private fog = new THREE.Fog(0x060910, 160, 420);
  private bg = C.dusk.clone();
  private bgTarget = C.night.clone();

  private plates: Layer; private tiles: Layer; private roads: Layer; private buildings: Layer;
  private ghosts: Layer; private towers: Layer; private beacons: Layer; private centers: Layer;
  private fins: Layer; private caches: Layer; private houses: Layer;

  private vehicleMesh: THREE.InstancedMesh;
  private particleMesh: THREE.InstancedMesh;
  private vehicles: Vehicle[] = [];
  private particles: Particle[] = [];
  private layouts = new Map<string, RegionLayout>();
  private health = new Map<string, RegionHealth>();
  private labels = new Map<string, HTMLDivElement>();
  private globalError = 0;
  private latencyDrag = 1;
  private dbRate = 0;
  private dbFail = 0;
  private dbAnchor = new THREE.Vector3();
  private flowFrom: THREE.Vector3[] = [];
  private cameraFit = 130;
  private userMoved = false;
  private framed = false;
  private raf = 0;
  private last = performance.now();
  private observer: ResizeObserver;
  private dummy = new THREE.Object3D();
  private litMaterial!: THREE.MeshStandardMaterial;
  private trafficScale = 1;

  constructor(private host: HTMLElement, private labelHost: HTMLElement) {
    this.renderer = new THREE.WebGLRenderer({ antialias: true, powerPreference: "high-performance" });
    this.renderer.setPixelRatio(Math.min(window.devicePixelRatio, 1.75));
    this.renderer.toneMapping = THREE.ACESFilmicToneMapping;
    this.renderer.toneMappingExposure = 1.05;
    host.appendChild(this.renderer.domElement);

    this.camera = new THREE.PerspectiveCamera(32, 1, 1, 900);
    this.camera.position.set(60, 70, 88);
    this.controls = new OrbitControls(this.camera, this.renderer.domElement);
    this.controls.enableDamping = true;
    this.controls.maxPolarAngle = 1.32;
    this.controls.minDistance = 30;
    this.controls.maxDistance = 420;
    this.controls.addEventListener("start", () => { this.userMoved = true; });

    this.scene.fog = this.fog;
    this.scene.add(this.hemi, this.sun);
    this.sun.position.set(60, 120, 40);

    const ground = new THREE.Mesh(new THREE.PlaneGeometry(1600, 1600), new THREE.MeshStandardMaterial({ color: 0x141a24, roughness: 1 }));
    ground.rotation.x = -Math.PI / 2;
    ground.position.y = -0.05;
    this.scene.add(ground);
    const grid = new THREE.GridHelper(1600, 160, 0x222c3b, 0x1b2330);
    grid.position.y = -0.02;
    this.scene.add(grid);

    const tex = windowTexture();
    const box = new THREE.BoxGeometry(1, 1, 1);
    const windowed = boxWithBlankRoof();
    const flat = (r = 0.9) => new THREE.MeshStandardMaterial({ color: 0xffffff, roughness: r });
    const glow = new THREE.MeshBasicMaterial({ color: 0xffffff, toneMapped: false });
    const lit = litBuildingMaterial(tex);
    this.litMaterial = lit;

    this.plates = new Layer(box, flat(), 8, this.scene);
    this.tiles = new Layer(box, flat(), 500, this.scene);
    this.roads = new Layer(box, flat(1), 60, this.scene);
    this.buildings = new Layer(windowed, lit, 1100, this.scene);
    this.ghosts = new Layer(box, new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.22, depthWrite: false }), 400, this.scene);
    this.towers = new Layer(windowed, lit, 80, this.scene);
    this.beacons = new Layer(new THREE.SphereGeometry(0.5, 12, 10), glow, 100, this.scene);
    this.centers = new Layer(windowed, lit, 24, this.scene);
    this.fins = new Layer(box, glow, 150, this.scene);
    this.caches = new Layer(new THREE.CylinderGeometry(0.5, 0.5, 1, 14), lit, 100, this.scene);
    this.houses = new Layer(windowed, lit, 900, this.scene);

    this.vehicleMesh = new THREE.InstancedMesh(new THREE.BoxGeometry(0.42, 0.26, 0.85), new THREE.MeshBasicMaterial({ color: 0xffffff, toneMapped: false }), MAX_VEHICLES);
    this.vehicleMesh.setColorAt(0, C.headlight);
    this.vehicleMesh.frustumCulled = false;
    this.vehicleMesh.count = 0;
    this.scene.add(this.vehicleMesh);

    this.particleMesh = new THREE.InstancedMesh(new THREE.SphereGeometry(0.2, 8, 6), new THREE.MeshBasicMaterial({ color: 0xffffff, toneMapped: false }), MAX_PARTICLES);
    this.particleMesh.setColorAt(0, C.flow);
    this.particleMesh.frustumCulled = false;
    this.particleMesh.count = 0;
    this.scene.add(this.particleMesh);

    this.composer = new EffectComposer(this.renderer);
    this.composer.addPass(new RenderPass(this.scene, this.camera));
    this.composer.addPass(new UnrealBloomPass(new THREE.Vector2(256, 256), 0.32, 0.5, 0.6));
    this.composer.addPass(new OutputPass());

    this.observer = new ResizeObserver(() => this.resize());
    this.observer.observe(host);
    this.resize();
    this.raf = requestAnimationFrame(this.loop);
  }

  private resize() {
    const w = this.host.clientWidth || 1;
    const h = this.host.clientHeight || 1;
    this.renderer.setSize(w, h);
    this.composer.setSize(w, h);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
  }

  sync(f: Frame) {
    const topo = f.topology;
    const health = f.health;
    if (!topo || !health) return;

    this.health = new Map((health.regions ?? []).map((r) => [r.name, r]));
    this.layouts.clear();
    for (const r of topo.regions) this.layouts.set(r.name, this.layoutRegion(r));

    const plates: Placement[] = [], tiles: Placement[] = [], roads: Placement[] = [], buildings: Placement[] = [];
    const ghosts: Placement[] = [], towers: Placement[] = [], beacons: Placement[] = [];
    const centers: Placement[] = [], fins: Placement[] = [], caches: Placement[] = [];

    for (const r of topo.regions) {
      this.placeRegion(r, this.health.get(r.name), f, { plates, tiles, roads, buildings, ghosts, towers, beacons, caches });
    }
    this.placeDatabase(f, centers, fins, beacons, roads);

    this.plates.sync(plates); this.tiles.sync(tiles); this.roads.sync(roads);
    this.buildings.sync(buildings); this.ghosts.sync(ghosts); this.towers.sync(towers);
    this.beacons.sync(beacons); this.centers.sync(centers); this.fins.sync(fins); this.caches.sync(caches);
    this.houses.sync(this.placeHouses(f));

    // Traffic drawn on a log scale: a city with 10x the users should look busier,
    // not be solid headlights from edge to edge.
    this.globalError = Math.min(1, health.errorRate);
    this.latencyDrag = 1 + Math.min(4, health.latencyP95Ms / 400);
    this.dbRate = Math.min(1, (health.database?.utilization ?? 0) / 1.1);
    this.dbFail = health.database?.errorRate ?? 0;

    // Dusk at peak traffic, deep night at the trough, and a sick red cast in an outage.
    const dayness = Math.max(0, Math.min(1, (health.diurnal - 0.55) / 0.9));
    this.bgTarget.copy(C.night).lerp(C.dusk, dayness).lerp(C.alarm, Math.min(0.75, this.globalError * 1.4));
    this.hemi.intensity = 0.9 + 1.1 * dayness;
    this.sun.intensity = 0.7 + 1.5 * dayness;

    const reach = Math.max(...topo.regions.map((r) => {
      const [x, z] = SITES[r.name] ?? [0, 0];
      return Math.hypot(x, z);
    }), 0);
    const widest = Math.max(...[...this.layouts.values()].map((l) => l.w), 0);
    this.cameraFit = 96 + reach * 1.6 + widest * 2.4;
    this.updateLabels(topo.regions);

    // Open a saved game at the right height, instead of zooming out over ten seconds.
    if (!this.framed) {
      this.framed = true;
      const dir = this.camera.position.clone().sub(this.controls.target).normalize();
      this.camera.position.copy(this.controls.target).addScaledVector(dir, this.cameraFit);
    }
  }

  private layoutRegion(r: RegionState): RegionLayout {
    const [cx, cz] = SITES[r.name] ?? [0, 0];
    const nodes = Math.max(1, r.nodesReady + r.nodesProvisioning);
    const across = SLOTS_PER_SIDE[r.nodeSize] ?? 2;
    const deep = SLOTS_DEEP[r.nodeSize] ?? 2;
    const cols = Math.ceil(Math.sqrt(nodes));
    const rows = Math.ceil(nodes / cols);
    const w = cols * (across * SLOT + NODE_GAP) + 3;
    const d = rows * (deep * SLOT + NODE_GAP) + 3;
    return {
      name: r.name, cx, cz, w, d,
      entry: new THREE.Vector3(cx, 0.3, cz + d / 2 + 44),
      gate: new THREE.Vector3(cx, 0.3, cz + d / 2 + 6),
      core: new THREE.Vector3(cx, 0.3, cz + d / 2 - 0.5),
      dbAnchor: new THREE.Vector3(cx - w / 2 - 11, 0.3, cz - 2),
    };
  }

  private placeRegion(
    r: RegionState,
    h: RegionHealth | undefined,
    f: Frame,
    out: { plates: Placement[]; tiles: Placement[]; roads: Placement[]; buildings: Placement[]; ghosts: Placement[]; towers: Placement[]; beacons: Placement[]; caches: Placement[] },
  ) {
    const lay = this.layouts.get(r.name)!;
    const { cx, cz, w, d } = lay;
    const building = r.provisioningTicks > 0;
    const down = !building && h !== undefined && !h.up;
    const across = SLOTS_PER_SIDE[r.nodeSize] ?? 2;
    const deep = SLOTS_DEEP[r.nodeSize] ?? 2;

    out.plates.push({ key: r.name, x: cx, y: 0, z: cz, sx: w, sy: 0.7, sz: d, color: down ? C.plateDown : C.plate });
    out.roads.push({ key: `hwy-${r.name}`, x: cx, y: 0, z: cz + d / 2 + 25, sx: 3.2, sy: 0.12, sz: 44, color: C.road });

    const nodes = r.nodesReady + r.nodesProvisioning;
    const cols = Math.max(1, Math.ceil(Math.sqrt(Math.max(1, nodes))));
    const tileW = across * SLOT + NODE_GAP;
    const tileD = deep * SLOT + NODE_GAP;
    const originX = cx - (cols * tileW) / 2 + tileW / 2;
    const rows = Math.ceil(Math.max(1, nodes) / cols);
    const originZ = cz - (rows * tileD) / 2 + tileD / 2;

    const slots: [number, number][] = [];
    for (let n = 0; n < nodes; n++) {
      const tx = originX + (n % cols) * tileW;
      const tz = originZ + Math.floor(n / cols) * tileD;
      const booting = n >= r.nodesReady;
      out.tiles.push({
        key: `${r.name}-tile-${n}`, x: tx, y: 0.7, z: tz, sx: tileW - NODE_GAP * 0.6, sy: 0.12, sz: tileD - NODE_GAP * 0.6,
        color: booting ? C.tileBooting : down ? C.plateDown : C.tile,
      });
      if (booting) continue;
      for (let a = 0; a < across; a++) {
        for (let b = 0; b < deep; b++) {
          slots.push([tx - ((across - 1) * SLOT) / 2 + a * SLOT, tz - ((deep - 1) * SLOT) / 2 + b * SLOT]);
        }
      }
    }

    const util = h?.utilization ?? 0;
    const color = down ? C.dark : heat(util);
    let slot = 0;
    let waiting = 0;
    const newest = r.pods.reduce((v, p) => (p.version > v ? p.version : v), "");
    for (const p of r.pods) {
      if (p.phase === "pending") {
        // A translucent building hovering beside the campus, with nowhere to land.
        const col = waiting % 8;
        out.ghosts.push({ key: p.id, x: cx + w / 2 + 2 + (waiting >> 3) * 2.2, y: 0.8, z: cz - d / 2 + col * 2.2 + 1, sx: 1.6, sy: 3, sz: 1.6, color: C.ghost });
        waiting++;
        continue;
      }
      const at = slots[slot++];
      if (!at) continue;
      const seedH = hash(parseInt(p.id.slice(-4), 16) || slot);
      const height = 3.2 + seedH * 4.6 + (p.version === newest ? 0 : -0.4);
      const startingUp = p.phase === "starting";
      const crash = p.phase === "crashloop";
      out.buildings.push({
        key: p.id, x: at[0], y: 0.82, z: at[1], sx: SLOT * 0.72, sy: startingUp ? height * 0.4 : crash ? height * 0.65 : height, sz: SLOT * 0.72,
        color: crash ? C.hot : startingUp ? C.starting : color, blink: crash,
      });
    }

    // Towers guard the road in. Their beacon is the first thing that goes red when the
    // load balancers saturate, or when no client can finish a handshake at all.
    const tlsDead = (f.health?.ingress?.errorRate ?? 0) > 0.5;
    const towerHeat = heat(h?.towerUtilization ?? 0);
    for (let i = 0; i < r.towers; i++) {
      const side = i % 2 === 0 ? -1 : 1;
      const rank = Math.floor(i / 2);
      const tx = cx + side * (4 + rank * 3);
      const tz = lay.gate.z;
      out.towers.push({ key: `${r.name}-tower-${i}`, x: tx, y: 0, z: tz, sx: 1.5, sy: 12, sz: 1.5, color: down ? C.dark : towerHeat });
      out.beacons.push({ key: `${r.name}-beacon-${i}`, x: tx, y: 12.3, z: tz, sx: 1.2, sy: 1.2, sz: 1.2, color: down ? C.dark : tlsDead ? C.hot : towerHeat, blink: tlsDead && !down });
    }

    if (f.topology?.database?.primaryRegion === r.name || f.topology?.regions[0]?.name === r.name) {
      const cache = f.topology?.cache;
      const dead = (f.health?.cache?.errorRate ?? 0) > 0.5;
      const warmth = f.health?.cacheHitRatio ?? 0;
      for (let i = 0; i < (cache?.nodes ?? 0); i++) {
        const row = i % 3;
        out.caches.push({
          key: `cache-${i}`, x: cx + w / 2 + 7 + Math.floor(i / 3) * 3.2, y: 0, z: cz - 3 + row * 3.2, sx: 2.2, sy: cache?.replicated ? 4.6 : 3.2, sz: 2.2,
          color: dead ? C.dark : C.dark.clone().lerp(C.cacheOn, 0.25 + warmth * 0.75),
        });
      }
    }
  }

  private placeDatabase(f: Frame, centers: Placement[], fins: Placement[], beacons: Placement[], roads: Placement[]) {
    const db = f.topology?.database;
    if (!db) return;
    const primary = this.layouts.get(db.primaryRegion) ?? [...this.layouts.values()][0];
    if (!primary) return;
    const anchor = primary.dbAnchor;
    this.dbAnchor.copy(anchor);
    const util = f.health?.database?.utilization ?? 0;
    const failing = db.failoverTicks > 0;
    const dead = failing || (f.health?.database?.errorRate ?? 0) > 0.7;

    // Bigger tier, bigger building. A mem32 should look like it costs what it costs.
    const tiers = ["burst1", "gp2", "gp4", "gp8", "gp16", "mem32"];
    const rank = Math.max(0, tiers.indexOf(db.tier));
    const size = 6 + rank * 1.5;
    const col = dead ? C.hot : heat(util);
    centers.push({ key: "db", x: anchor.x, y: 0, z: anchor.z, sx: size * 1.25, sy: size * 0.8, sz: size, color: col, blink: dead });
    for (let i = 0; i < 3 + rank; i++) {
      fins.push({ key: `db-fin-${i}`, x: anchor.x - size * 0.5 + ((i + 0.5) * size) / (3 + rank), y: size * 0.8, z: anchor.z, sx: 0.35, sy: 0.5 + util * 1.1, sz: size * 0.7, color: col });
    }
    beacons.push({ key: "db-crown", x: anchor.x, y: size * 0.8 + 2.2, z: anchor.z, sx: 1, sy: 1, sz: 1, color: col, blink: dead });

    for (let i = 0; i < db.readReplicas; i++) {
      centers.push({ key: `db-replica-${i}`, x: anchor.x - 1 - i * 5.2, y: 0, z: anchor.z + size * 0.5 + 4.5, sx: 4.2, sy: 3.4, sz: 4.2, color: heat(util * 0.8) });
    }
    if (db.standbyRegion && this.layouts.get(db.standbyRegion)) {
      const a = this.layouts.get(db.standbyRegion)!.dbAnchor;
      centers.push({ key: "db-standby", x: a.x, y: 0, z: a.z, sx: size, sy: size * 0.6, sz: size * 0.8, color: C.standby });
    }

    this.flowFrom = [];
    for (const lay of this.layouts.values()) {
      const h = this.health.get(lay.name);
      if (!h || h.up) this.flowFrom.push(new THREE.Vector3(lay.cx, 2, lay.cz));
      // Service road from campus to whichever data center it depends on.
      const dx = anchor.x - lay.cx, dz = anchor.z - lay.cz;
      if (lay === primary) {
        roads.push({ key: `svc-${lay.name}`, x: (lay.cx + anchor.x) / 2, y: 0, z: (lay.cz + anchor.z) / 2, sx: Math.hypot(dx, dz), sy: 0.1, sz: 1.2, color: C.road, rotY: -Math.atan2(dz, dx) });
      }
    }
  }

  // The suburbs are the only part of the city you don't pay for. They're there so
  // that growth looks like something happened, and to show you who you're paging for.
  private placeHouses(f: Frame): Placement[] {
    const users = f.ledger?.users ?? 0;
    const n = Math.min(880, Math.floor(30 + Math.pow(Math.max(users, 1), 0.52)));
    const regions = [...this.layouts.values()];
    const out: Placement[] = [];
    for (let i = 0; i < n && regions.length; i++) {
      const lay = regions[i % regions.length];
      const angle = hash(i * 3.1) * Math.PI * 0.75 + Math.PI * 0.125;
      const radius = 28 + lay.d / 2 + hash(i * 7.7) * (34 + Math.sqrt(i) * 1.5);
      const x = lay.cx + Math.cos(angle) * radius * 1.3;
      const z = lay.cz + Math.sin(angle) * radius;
      if (Math.abs(x - lay.cx) < 3.4) continue; // keep the highway clear
      const h = 0.8 + hash(i * 1.9) * 1.1;
      out.push({ key: `h${i}`, x, y: 0, z, sx: 1.1, sy: h, sz: 1.1, color: C.house.clone().multiplyScalar(0.35 + hash(i) * 0.5) });
    }
    return out;
  }

  private updateLabels(regions: RegionState[]) {
    for (const [name, el] of this.labels) {
      if (!regions.some((r) => r.name === name)) {
        el.remove();
        this.labels.delete(name);
      }
    }
    for (const r of regions) {
      let el = this.labels.get(r.name);
      if (!el) {
        el = document.createElement("div");
        el.className = "city-label";
        this.labelHost.appendChild(el);
        this.labels.set(r.name, el);
      }
      const h = this.health.get(r.name);
      const ready = h?.podsReady ?? r.pods.filter((p) => p.phase === "ready").length;
      let state = `${ready}/${r.pods.length} pods`;
      if (r.provisioningTicks > 0) state = `building, ${r.provisioningTicks} ticks left`;
      else if (h && !h.up) state = "region unavailable";
      else if ((h?.podsPending ?? 0) > 0) state += `, ${h?.podsPending} pending`;
      el.textContent = `${r.name} · ${state}`;
      el.dataset.tone = h && !h.up ? "critical" : (h?.podsPending ?? 0) > 0 ? "warning" : "";
    }
  }

  private projected = new THREE.Vector3();
  private positionLabels() {
    const w = this.host.clientWidth, h = this.host.clientHeight;
    for (const [name, el] of this.labels) {
      const lay = this.layouts.get(name);
      if (!lay) continue;
      this.projected.set(lay.cx, 0, lay.cz + lay.d / 2 + 1).project(this.camera);
      el.style.transform = `translate(-50%, 0) translate(${((this.projected.x + 1) / 2) * w}px, ${((1 - this.projected.y) / 2) * h}px)`;
    }
  }

  private spawnTraffic(dt: number) {
    const target = (rps: number) => Math.min(170, Math.round(6 + 34 * Math.log10(1 + rps / 8)));
    for (const [name, h] of this.health) {
      if (!h.up || !this.layouts.has(name)) continue;
      const have = this.vehicles.filter((v) => v.region === name).length;
      const want = target(h.offeredRps);
      if (have < want && Math.random() < dt * 14 && this.vehicles.length < MAX_VEHICLES) {
        const errorChance = Math.max(this.globalError, h.errorRate);
        this.vehicles.push({
          region: name, t: 0, speed: 0.12 + Math.random() * 0.05,
          failing: Math.random() < errorChance, lane: Math.random() < 0.5 ? -0.75 : 0.75, lane2: Math.random(), dead: false,
        });
      }
    }
    const wantFlow = Math.round(this.dbRate * 90);
    if (this.particles.length < wantFlow && this.flowFrom.length && Math.random() < dt * 30 && this.particles.length < MAX_PARTICLES) {
      const from = this.flowFrom[Math.floor(Math.random() * this.flowFrom.length)];
      const to = this.dbAnchor.clone().setY(2);
      this.particles.push({ from, to, t: 0, speed: 0.5 + Math.random() * 0.4, failing: Math.random() < this.dbFail, arc: 2 + from.distanceTo(to) * 0.18 });
    }
  }

  private stepTraffic(dt: number) {
    const d = this.dummy;
    let i = 0;
    for (const v of this.vehicles) {
      const lay = this.layouts.get(v.region);
      const h = this.health.get(v.region);
      if (!lay || !h || !h.up) { v.dead = true; continue; }
      // Saturated regions queue visibly: traffic crawls and stacks up at the towers.
      const crawl = v.t > 0.5 ? 1 / this.latencyDrag : 1;
      const queue = h.utilization > 1 && v.t > 0.72 ? 0.3 : 1;
      v.t += dt * v.speed * crawl * queue;
      if (v.t >= 1) { v.dead = true; continue; }
      // Failed requests die at the gate. A pile of red at the towers means rejected.
      if (v.failing && v.t > 0.82) { v.dead = true; continue; }
      const p = v.t < 0.86
        ? d.position.lerpVectors(lay.entry, lay.gate, v.t / 0.86)
        : d.position.lerpVectors(lay.gate, lay.core, (v.t - 0.86) / 0.14);
      p.x += v.lane;
      d.rotation.set(0, 0, 0);
      d.scale.setScalar(this.trafficScale);
      d.updateMatrix();
      this.vehicleMesh.setMatrixAt(i, d.matrix);
      this.vehicleMesh.setColorAt(i, v.failing ? C.failing : C.headlight);
      i++;
    }
    this.vehicleMesh.count = i;
    this.vehicleMesh.instanceMatrix.needsUpdate = true;
    if (this.vehicleMesh.instanceColor) this.vehicleMesh.instanceColor.needsUpdate = true;
    this.vehicles = this.vehicles.filter((v) => !v.dead);

    i = 0;
    for (const p of this.particles) {
      p.t += dt * p.speed;
      if (p.t >= 1) continue;
      d.position.lerpVectors(p.from, p.to, p.t);
      d.position.y += Math.sin(p.t * Math.PI) * p.arc;
      d.scale.setScalar(this.trafficScale);
      d.updateMatrix();
      this.particleMesh.setMatrixAt(i, d.matrix);
      this.particleMesh.setColorAt(i, p.failing ? C.failing : C.flow);
      i++;
    }
    this.particleMesh.count = i;
    this.particleMesh.instanceMatrix.needsUpdate = true;
    if (this.particleMesh.instanceColor) this.particleMesh.instanceColor.needsUpdate = true;
    this.particles = this.particles.filter((p) => p.t < 1);
  }

  // Pulled back far enough, a window is less than a pixel and averages down to black,
  // cars vanish and fog eats the far district. A real tilt-shift miniature doesn't have
  // that problem, so we cheat toward what you'd expect to see from up here.
  private adaptToDistance() {
    const dist = this.camera.position.distanceTo(this.controls.target);
    this.fog.near = dist * 1.3;
    this.fog.far = dist * 3.4;
    this.litMaterial.emissiveIntensity = 0.85 * Math.min(3.2, Math.max(1, dist / 130) ** 1.6);
    this.trafficScale = Math.min(2.4, Math.max(1, dist / 130));
  }

  private loop = (now: number) => {
    this.raf = requestAnimationFrame(this.loop);
    const dt = Math.min(0.05, (now - this.last) / 1000);
    this.last = now;
    const time = now / 1000;

    for (const layer of [this.plates, this.tiles, this.roads, this.buildings, this.ghosts, this.towers, this.beacons, this.centers, this.fins, this.caches, this.houses]) {
      layer.animate(dt, time);
    }
    this.spawnTraffic(dt);
    this.stepTraffic(dt);

    this.bg.lerp(this.bgTarget, 1 - Math.exp(-dt * 1.2));
    this.scene.background = this.bg;
    this.fog.color.copy(this.bg);

    // The camera backs off as the platform grows, until you take the wheel yourself.
    if (!this.userMoved) {
      const dir = this.camera.position.clone().sub(this.controls.target).normalize();
      const dist = this.camera.position.distanceTo(this.controls.target);
      const next = dist + (this.cameraFit - dist) * (1 - Math.exp(-dt * 0.8));
      this.camera.position.copy(this.controls.target).addScaledVector(dir, next);
    }
    this.controls.update();
    this.adaptToDistance();
    this.positionLabels();
    this.composer.render(dt);
  };

  dispose() {
    cancelAnimationFrame(this.raf);
    this.observer.disconnect();
    this.controls.dispose();
    for (const el of this.labels.values()) el.remove();
    this.composer.dispose();
    this.renderer.dispose();
    this.renderer.domElement.remove();
  }
}
