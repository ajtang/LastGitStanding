import * as THREE from "three";

export interface Placement {
  key: string;
  x: number; y: number; z: number;
  sx: number; sy: number; sz: number;
  color: THREE.Color;
  rotY?: number;
  blink?: boolean; // for things that want your attention specifically
}

interface Item extends Placement {
  cur: { x: number; y: number; z: number; sx: number; sy: number; sz: number; color: THREE.Color };
  gone: boolean;
}

const dummy = new THREE.Object3D();
const off = new THREE.Color("#000000");
const scratch = new THREE.Color();

// Layer is the whole trick behind the city: each sync says what should exist, keyed,
// and each frame everything eases toward that. New things grow out of the ground and
// removed things sink back into it, without a single line of per-object animation.
export class Layer {
  readonly mesh: THREE.InstancedMesh;
  private items = new Map<string, Item>();

  constructor(geometry: THREE.BufferGeometry, material: THREE.Material, private capacity: number, parent: THREE.Object3D) {
    this.mesh = new THREE.InstancedMesh(geometry, material, capacity);
    this.mesh.instanceMatrix.setUsage(THREE.DynamicDrawUsage);
    this.mesh.setColorAt(0, off); // forces the instanceColor attribute into existence
    this.mesh.frustumCulled = false;
    this.mesh.count = 0;
    parent.add(this.mesh);
  }

  sync(list: Placement[]) {
    const seen = new Set<string>();
    for (const p of list) {
      seen.add(p.key);
      const existing = this.items.get(p.key);
      if (existing) {
        Object.assign(existing, p, { gone: false, cur: existing.cur });
      } else if (this.items.size < this.capacity) {
        this.items.set(p.key, {
          ...p,
          gone: false,
          cur: { x: p.x, y: p.y, z: p.z, sx: p.sx, sy: 0.001, sz: p.sz, color: p.color.clone() },
        });
      }
    }
    for (const [key, item] of this.items) {
      if (!seen.has(key)) item.gone = true;
    }
  }

  animate(dt: number, time: number) {
    const k = 1 - Math.exp(-dt * 3.2);
    let i = 0;
    for (const [key, it] of this.items) {
      const c = it.cur;
      const targetSy = it.gone ? 0 : it.sy;
      c.x += (it.x - c.x) * k;
      c.z += (it.z - c.z) * k;
      c.sx += (it.sx - c.sx) * k;
      c.sz += (it.sz - c.sz) * k;
      c.sy += (targetSy - c.sy) * k;
      c.y += (it.y - c.y) * k;
      c.color.lerp(it.color, k);
      if (it.gone && c.sy < 0.02) {
        this.items.delete(key);
        continue;
      }
      // Anchored at the base: a resized building grows up, not out of the floor.
      dummy.position.set(c.x, c.y + c.sy / 2, c.z);
      dummy.scale.set(c.sx, Math.max(c.sy, 0.001), c.sz);
      dummy.rotation.set(0, it.rotY ?? 0, 0);
      dummy.updateMatrix();
      this.mesh.setMatrixAt(i, dummy.matrix);
      if (it.blink) {
        const pulse = 0.55 + 0.45 * Math.sin(time * 7 + i);
        this.mesh.setColorAt(i, scratch.copy(c.color).multiplyScalar(pulse));
      } else {
        this.mesh.setColorAt(i, c.color);
      }
      i++;
    }
    this.mesh.count = i;
    this.mesh.instanceMatrix.needsUpdate = true;
    if (this.mesh.instanceColor) this.mesh.instanceColor.needsUpdate = true;
  }
}
