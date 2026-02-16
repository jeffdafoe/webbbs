<?php

namespace App\Door\ZChat\Entity;

use App\Door\ZChat\Entity\Enum\RoomType;
use App\Door\ZChat\Repository\RoomRepository;
use App\Entity\Board;
use App\Entity\User;
use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\Common\Collections\Collection;
use Doctrine\DBAL\Types\Types;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: RoomRepository::class)]
#[ORM\Table(name: 'zchat_room', schema: 'doors')]
#[ORM\HasLifecycleCallbacks]
#[ORM\UniqueConstraint(name: 'zchat_unique_board_room_slug', columns: ['board_id', 'slug'])]
class Room
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: true)]
    private ?Board $board = null;

    #[ORM\Column(length: 50)]
    private string $slug;

    #[ORM\Column(length: 50)]
    private string $name;

    #[ORM\Column(type: Types::TEXT, nullable: true)]
    private ?string $description = null;

    #[ORM\Column(type: Types::STRING, length: 20, enumType: RoomType::class)]
    private RoomType $roomType = RoomType::PUBLIC;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: true)]
    private ?User $roomLeader = null;

    #[ORM\Column]
    private int $minSecurityLevel = 0;

    #[ORM\Column]
    private int $maxSecurityLevel = 255;

    #[ORM\Column(nullable: true)]
    private ?int $minAge = null;

    #[ORM\Column(nullable: true)]
    private ?int $maxAge = null;

    #[ORM\Column]
    private bool $noAccessCanSee = true;

    #[ORM\Column(length: 50)]
    private string $actionListSlug = 'default';

    #[ORM\Column(nullable: true)]
    private ?int $maxUsers = null;

    #[ORM\Column]
    private bool $isActive = true;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    #[ORM\Column]
    private \DateTimeImmutable $updatedAt;

    /** @var Collection<int, Message> */
    #[ORM\OneToMany(targetEntity: Message::class, mappedBy: 'room', orphanRemoval: true)]
    #[ORM\OrderBy(['createdAt' => 'DESC'])]
    private Collection $messages;

    /** @var Collection<int, Presence> */
    #[ORM\OneToMany(targetEntity: Presence::class, mappedBy: 'room')]
    private Collection $presences;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
        $this->updatedAt = new \DateTimeImmutable();
        $this->messages = new ArrayCollection();
        $this->presences = new ArrayCollection();
    }

    #[ORM\PreUpdate]
    public function onPreUpdate(): void
    {
        $this->updatedAt = new \DateTimeImmutable();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getBoard(): ?Board
    {
        return $this->board;
    }

    public function setBoard(?Board $board): static
    {
        $this->board = $board;
        return $this;
    }

    public function getSlug(): string
    {
        return $this->slug;
    }

    public function setSlug(string $slug): static
    {
        $this->slug = $slug;
        return $this;
    }

    public function getName(): string
    {
        return $this->name;
    }

    public function setName(string $name): static
    {
        $this->name = $name;
        return $this;
    }

    public function getDescription(): ?string
    {
        return $this->description;
    }

    public function setDescription(?string $description): static
    {
        $this->description = $description;
        return $this;
    }

    public function getRoomType(): RoomType
    {
        return $this->roomType;
    }

    public function setRoomType(RoomType $roomType): static
    {
        $this->roomType = $roomType;
        return $this;
    }

    public function getRoomLeader(): ?User
    {
        return $this->roomLeader;
    }

    public function setRoomLeader(?User $roomLeader): static
    {
        $this->roomLeader = $roomLeader;
        return $this;
    }

    public function getMinSecurityLevel(): int
    {
        return $this->minSecurityLevel;
    }

    public function setMinSecurityLevel(int $minSecurityLevel): static
    {
        $this->minSecurityLevel = $minSecurityLevel;
        return $this;
    }

    public function getMaxSecurityLevel(): int
    {
        return $this->maxSecurityLevel;
    }

    public function setMaxSecurityLevel(int $maxSecurityLevel): static
    {
        $this->maxSecurityLevel = $maxSecurityLevel;
        return $this;
    }

    public function getMinAge(): ?int
    {
        return $this->minAge;
    }

    public function setMinAge(?int $minAge): static
    {
        $this->minAge = $minAge;
        return $this;
    }

    public function getMaxAge(): ?int
    {
        return $this->maxAge;
    }

    public function setMaxAge(?int $maxAge): static
    {
        $this->maxAge = $maxAge;
        return $this;
    }

    public function isNoAccessCanSee(): bool
    {
        return $this->noAccessCanSee;
    }

    public function setNoAccessCanSee(bool $noAccessCanSee): static
    {
        $this->noAccessCanSee = $noAccessCanSee;
        return $this;
    }

    public function getActionListSlug(): string
    {
        return $this->actionListSlug;
    }

    public function setActionListSlug(string $actionListSlug): static
    {
        $this->actionListSlug = $actionListSlug;
        return $this;
    }

    public function getMaxUsers(): ?int
    {
        return $this->maxUsers;
    }

    public function setMaxUsers(?int $maxUsers): static
    {
        $this->maxUsers = $maxUsers;
        return $this;
    }

    public function isActive(): bool
    {
        return $this->isActive;
    }

    public function setIsActive(bool $isActive): static
    {
        $this->isActive = $isActive;
        return $this;
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }

    public function getUpdatedAt(): \DateTimeImmutable
    {
        return $this->updatedAt;
    }

    /** @return Collection<int, Message> */
    public function getMessages(): Collection
    {
        return $this->messages;
    }

    /** @return Collection<int, Presence> */
    public function getPresences(): Collection
    {
        return $this->presences;
    }

    public function getUserCount(): int
    {
        return $this->presences->count();
    }

    public function isPublic(): bool
    {
        return $this->roomType === RoomType::PUBLIC || $this->roomType === RoomType::PERMANENT;
    }
}
